package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

//go:embed templates/*.html db/schema.sql
var embeddedFiles embed.FS

type app struct {
	pool      *pgxpool.Pool
	templates *template.Template
}

type video struct {
	ID        int64
	VideoURL  string
	CreatedAt time.Time
}

type videoView struct {
	ID       int64
	VideoURL string
	Empty    bool
	Message  string
}

type pageData struct {
	Video videoView
}

type user struct {
	ID        int64     `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

type authRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type authResponse struct {
	Message string `json:"message"`
	Token   string `json:"token,omitempty"`
	User    *user  `json:"user,omitempty"`
}

type claims struct {
	UserID   int64  `json:"user_id"`
	Username string `json:"username"`
	Email    string `json:"email"`
	jwt.RegisteredClaims
}

func main() {
	ctx := context.Background()

	config, err := pgxpool.ParseConfig(databaseURL())
	if err != nil {
		log.Fatalf("parse database config: %v", err)
	}

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		log.Fatalf("open database: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping database: %v", err)
	}

	server, err := newApp(pool)
	if err != nil {
		log.Fatalf("build app: %v", err)
	}

	if err := server.ensureSchema(ctx); err != nil {
		log.Fatalf("ensure schema: %v", err)
	}

	if err := server.seedVideos(ctx); err != nil {
		log.Fatalf("seed videos: %v", err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", server.handleHome)
	mux.HandleFunc("/video", server.handleVideo)
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("POST /api/auth/register", server.handleRegister)
	mux.HandleFunc("POST /api/auth/login", server.handleLogin)
	mux.HandleFunc("POST /api/auth/logout", server.handleLogout)
	mux.Handle("GET /api/auth/me", requireAuth(server.handleMe))

	httpServer := &http.Server{
		Addr:         ":" + env("PORT", "8080"),
		Handler:      logRequests(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("listening on %s", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve http: %v", err)
	}
}

func newApp(pool *pgxpool.Pool) (*app, error) {
	templates, err := template.ParseFS(embeddedFiles, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse templates: %w", err)
	}

	return &app{
		pool:      pool,
		templates: templates,
	}, nil
}

func (a *app) ensureSchema(ctx context.Context) error {
	schema, err := embeddedFiles.ReadFile("db/schema.sql")
	if err != nil {
		return fmt.Errorf("read schema: %w", err)
	}

	if _, err := a.pool.Exec(ctx, string(schema)); err != nil {
		return fmt.Errorf("run schema: %w", err)
	}

	return nil
}

func (a *app) seedVideos(ctx context.Context) error {
	raw := strings.TrimSpace(os.Getenv("SEED_VIDEO_URLS"))
	if raw == "" {
		return nil
	}

	parts := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n'
	})

	for _, part := range parts {
		videoURL := strings.TrimSpace(part)
		if videoURL == "" {
			continue
		}

		if _, err := a.pool.Exec(
			ctx,
			`INSERT INTO videos (video_url) VALUES ($1) ON CONFLICT (video_url) DO NOTHING`,
			videoURL,
		); err != nil {
			return fmt.Errorf("insert seed video %q: %w", videoURL, err)
		}
	}

	return nil
}

func (a *app) handleHome(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	view, err := a.loadInitialVideo(r.Context(), parseInt64(r.URL.Query().Get("video")))
	if err != nil {
		log.Printf("load home video: %v", err)
		http.Error(w, "failed to load videos", http.StatusInternalServerError)
		return
	}

	a.render(w, http.StatusOK, "index.html", pageData{Video: view})
}

func (a *app) handleVideo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	currentID := parseInt64(query.Get("current_id"))
	requestedID := parseInt64(query.Get("id"))
	direction := strings.ToLower(strings.TrimSpace(query.Get("direction")))

	var (
		view videoView
		err  error
	)

	switch {
	case requestedID != nil:
		view, err = a.loadInitialVideo(r.Context(), requestedID)
	case direction == "prev":
		view, err = a.loadPreviousVideo(r.Context(), currentID)
	default:
		view, err = a.loadNextVideo(r.Context(), currentID)
	}

	if err != nil {
		log.Printf("load video partial: %v", err)
		http.Error(w, "failed to load video", http.StatusInternalServerError)
		return
	}

	a.render(w, http.StatusOK, "video.html", view)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (a *app) loadInitialVideo(ctx context.Context, requestedID *int64) (videoView, error) {
	if requestedID != nil {
		item, err := a.getVideoByID(ctx, *requestedID)
		if err == nil {
			return makeVideoView(item), nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return videoView{}, err
		}
	}

	item, err := a.getFirstVideo(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyVideoView(), nil
	}
	if err != nil {
		return videoView{}, err
	}

	return makeVideoView(item), nil
}

func (a *app) loadNextVideo(ctx context.Context, currentID *int64) (videoView, error) {
	if currentID == nil {
		return a.loadInitialVideo(ctx, nil)
	}

	item, err := a.getNextVideo(ctx, *currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyVideoView(), nil
	}
	if err != nil {
		return videoView{}, err
	}

	return makeVideoView(item), nil
}

func (a *app) loadPreviousVideo(ctx context.Context, currentID *int64) (videoView, error) {
	if currentID == nil {
		return a.loadInitialVideo(ctx, nil)
	}

	item, err := a.getPreviousVideo(ctx, *currentID)
	if errors.Is(err, pgx.ErrNoRows) {
		return emptyVideoView(), nil
	}
	if err != nil {
		return videoView{}, err
	}

	return makeVideoView(item), nil
}

func (a *app) getFirstVideo(ctx context.Context) (video, error) {
	return a.queryVideo(
		ctx,
		`SELECT id, video_url, created_at
		FROM videos
		ORDER BY id ASC
		LIMIT 1`,
	)
}

func (a *app) getLastVideo(ctx context.Context) (video, error) {
	return a.queryVideo(
		ctx,
		`SELECT id, video_url, created_at
		FROM videos
		ORDER BY id DESC
		LIMIT 1`,
	)
}

func (a *app) getVideoByID(ctx context.Context, id int64) (video, error) {
	return a.queryVideo(
		ctx,
		`SELECT id, video_url, created_at
		FROM videos
		WHERE id = $1`,
		id,
	)
}

func (a *app) getNextVideo(ctx context.Context, currentID int64) (video, error) {
	item, err := a.queryVideo(
		ctx,
		`SELECT id, video_url, created_at
		FROM videos
		WHERE id > $1
		ORDER BY id ASC
		LIMIT 1`,
		currentID,
	)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return video{}, err
	}

	return a.getFirstVideo(ctx)
}

func (a *app) getPreviousVideo(ctx context.Context, currentID int64) (video, error) {
	item, err := a.queryVideo(
		ctx,
		`SELECT id, video_url, created_at
		FROM videos
		WHERE id < $1
		ORDER BY id DESC
		LIMIT 1`,
		currentID,
	)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return video{}, err
	}

	return a.getLastVideo(ctx)
}

func (a *app) queryVideo(ctx context.Context, sql string, args ...any) (video, error) {
	var item video

	err := a.pool.QueryRow(ctx, sql, args...).Scan(&item.ID, &item.VideoURL, &item.CreatedAt)
	if err != nil {
		return video{}, err
	}

	return item, nil
}

func (a *app) render(w http.ResponseWriter, status int, templateName string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)

	if err := a.templates.ExecuteTemplate(w, templateName, data); err != nil {
		log.Printf("render %s: %v", templateName, err)
	}
}

func makeVideoView(item video) videoView {
	return videoView{
		ID:       item.ID,
		VideoURL: item.VideoURL,
	}
}

func emptyVideoView() videoView {
	return videoView{
		Empty:   true,
		Message: "No videos yet. Add Cloudflare CDN or R2 links into Postgres.",
	}
}

func parseInt64(raw string) *int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}

	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}

	return &value
}

func databaseURL() string {
	if value := strings.TrimSpace(os.Getenv("DATABASE_URL")); value != "" {
		return value
	}

	host := env("DB_HOST", "127.0.0.1")
	port := env("DB_PORT", "5432")
	user := env("DB_USER", "postgres")
	password := os.Getenv("DB_PASSWORD")
	name := env("DB_NAME", "postgres")
	sslMode := env("DB_SSLMODE", "disable")

	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		user,
		password,
		host,
		port,
		name,
		sslMode,
	)
}

func env(key string, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	return fallback
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		startedAt := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(startedAt).Round(time.Millisecond))
	})
}

// Authentication handlers
func (a *app) handleRegister(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "method not allowed"})
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, authResponse{Message: "invalid request body"})
		return
	}

	if req.Username == "" || req.Email == "" || req.Password == "" {
		respondJSON(w, http.StatusBadRequest, authResponse{Message: "username, email, and password are required"})
		return
	}

	hashedPassword, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to hash password"})
		return
	}

	var userID int64
	err = a.pool.QueryRow(
		r.Context(),
		`INSERT INTO users (username, email, password) VALUES ($1, $2, $3) RETURNING id`,
		req.Username,
		req.Email,
		string(hashedPassword),
	).Scan(&userID)

	if err != nil {
		if strings.Contains(err.Error(), "unique constraint") {
			respondJSON(w, http.StatusConflict, authResponse{Message: "username or email already exists"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to create user"})
		return
	}

	usr := &user{
		ID:        userID,
		Username:  req.Username,
		Email:     req.Email,
		CreatedAt: time.Now(),
	}

	token, err := generateToken(userID, req.Username, req.Email)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to generate token"})
		return
	}

	respondJSON(w, http.StatusCreated, authResponse{
		Message: "user registered successfully",
		Token:   token,
		User:    usr,
	})
}

func (a *app) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "method not allowed"})
		return
	}

	var req authRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondJSON(w, http.StatusBadRequest, authResponse{Message: "invalid request body"})
		return
	}

	if req.Email == "" || req.Password == "" {
		respondJSON(w, http.StatusBadRequest, authResponse{Message: "email and password are required"})
		return
	}

	var (
		userID       int64
		username     string
		storedHash   string
	)

	err := a.pool.QueryRow(
		r.Context(),
		`SELECT id, username, password FROM users WHERE email = $1`,
		req.Email,
	).Scan(&userID, &username, &storedHash)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondJSON(w, http.StatusUnauthorized, authResponse{Message: "invalid email or password"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to authenticate"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(storedHash), []byte(req.Password)); err != nil {
		respondJSON(w, http.StatusUnauthorized, authResponse{Message: "invalid email or password"})
		return
	}

	usr := &user{
		ID:        userID,
		Username:  username,
		Email:     req.Email,
		CreatedAt: time.Now(),
	}

	token, err := generateToken(userID, username, req.Email)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to generate token"})
		return
	}

	respondJSON(w, http.StatusOK, authResponse{
		Message: "logged in successfully",
		Token:   token,
		User:    usr,
	})
}

func (a *app) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		respondJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "method not allowed"})
		return
	}

	respondJSON(w, http.StatusOK, authResponse{Message: "logged out successfully"})
}

func (a *app) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		respondJSON(w, http.StatusMethodNotAllowed, authResponse{Message: "method not allowed"})
		return
	}

	c, ok := r.Context().Value("claims").(*claims)
	if !ok {
		respondJSON(w, http.StatusUnauthorized, authResponse{Message: "invalid token"})
		return
	}

	var createdAt time.Time
	err := a.pool.QueryRow(
		r.Context(),
		`SELECT created_at FROM users WHERE id = $1`,
		c.UserID,
	).Scan(&createdAt)

	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			respondJSON(w, http.StatusNotFound, authResponse{Message: "user not found"})
			return
		}
		respondJSON(w, http.StatusInternalServerError, authResponse{Message: "failed to fetch user"})
		return
	}

	usr := &user{
		ID:        c.UserID,
		Username:  c.Username,
		Email:     c.Email,
		CreatedAt: createdAt,
	}

	respondJSON(w, http.StatusOK, authResponse{
		Message: "user info retrieved successfully",
		User:    usr,
	})
}

// Authentication middleware
func requireAuth(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			respondJSON(w, http.StatusUnauthorized, authResponse{Message: "missing authorization header"})
			return
		}

		parts := strings.Split(authHeader, " ")
		if len(parts) != 2 || parts[0] != "Bearer" {
			respondJSON(w, http.StatusUnauthorized, authResponse{Message: "invalid authorization header"})
			return
		}

		tokenString := parts[1]
		c := &claims{}

		token, err := jwt.ParseWithClaims(tokenString, c, func(token *jwt.Token) (interface{}, error) {
			if _, ok := token.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected signing method: %v", token.Header["alg"])
			}
			return getJWTSecret(), nil
		})

		if err != nil || !token.Valid {
			respondJSON(w, http.StatusUnauthorized, authResponse{Message: "invalid or expired token"})
			return
		}

		ctx := context.WithValue(r.Context(), "claims", c)
		next(w, r.WithContext(ctx))
	})
}

// JWT and password helpers
func generateToken(userID int64, username string, email string) (string, error) {
	now := time.Now()
	c := claims{
		UserID:   userID,
		Username: username,
		Email:    email,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(now.Add(24 * time.Hour)),
		},
	}

	token := jwt.NewWithClaims(jwt.SigningMethodHS256, c)
	return token.SignedString(getJWTSecret())
}

func getJWTSecret() []byte {
	secret := env("JWT_SECRET", "dev-secret-key-not-for-production")
	return []byte(secret)
}

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}
