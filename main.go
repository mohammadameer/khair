package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ─── YouTube types ───────────────────────────────────────────────────────────

type YouTubeVideo struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Thumbnail   string `json:"thumbnail"`
	ChannelName string `json:"channel_name"`
	ViewCount   string `json:"view_count"`
	EmbedURL    string `json:"embed_url"`
}

type youtubeSearchResponse struct {
	Items []struct {
		ID struct {
			VideoID string `json:"videoId"`
		} `json:"id"`
		Snippet struct {
			Title        string `json:"title"`
			ChannelTitle string `json:"channelTitle"`
			Thumbnails   struct {
				High struct {
					URL string `json:"url"`
				} `json:"high"`
			} `json:"thumbnails"`
		} `json:"snippet"`
	} `json:"items"`
	NextPageToken string `json:"nextPageToken"`
}

type youtubeStatsResponse struct {
	Items []struct {
		ID         string `json:"id"`
		Statistics struct {
			ViewCount string `json:"viewCount"`
		} `json:"statistics"`
		ContentDetails struct {
			Duration string `json:"duration"`
		} `json:"contentDetails"`
	} `json:"items"`
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/api/youtube/shorts", handleYoutubeShorts)

	httpServer := &http.Server{
		Addr:         ":" + env("PORT", "8080"),
		Handler:      corsMiddleware(logRequests(mux)),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	log.Printf("listening on %s", httpServer.Addr)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve http: %v", err)
	}
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func handleYoutubeShorts(w http.ResponseWriter, r *http.Request) {
	apiKey := os.Getenv("YOUTUBE_API_KEY")
	if apiKey == "" {
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "YouTube API key not configured",
		})
		return
	}

	pageToken := r.URL.Query().Get("pageToken")

	// Step 1: Search trending short videos
	params := url.Values{}
	params.Set("part", "snippet")
	params.Set("chart", "mostPopular")
	params.Set("type", "video")
	params.Set("videoDuration", "short")
	params.Set("maxResults", "20")
	params.Set("key", apiKey)
	if pageToken != "" {
		params.Set("pageToken", pageToken)
	}

	searchURL := "https://www.googleapis.com/youtube/v3/search?" + params.Encode()
	searchData, err := fetchJSON(searchURL)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to fetch from YouTube: " + err.Error(),
		})
		return
	}

	var searchResult youtubeSearchResponse
	if err := json.Unmarshal(searchData, &searchResult); err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to parse YouTube response",
		})
		return
	}

	if len(searchResult.Items) == 0 {
		respondJSON(w, http.StatusOK, map[string]interface{}{
			"videos":         []interface{}{},
			"nextPageToken":  "",
		})
		return
	}

	// Step 2: Collect video IDs
	ids := make([]string, 0, len(searchResult.Items))
	for _, item := range searchResult.Items {
		ids = append(ids, item.ID.VideoID)
	}

	// Step 3: Fetch duration + stats
	statsURL := fmt.Sprintf(
		"https://www.googleapis.com/youtube/v3/videos?part=statistics,contentDetails&id=%s&key=%s",
		strings.Join(ids, ","), apiKey,
	)
	statsData, err := fetchJSON(statsURL)
	if err != nil {
		respondJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "failed to fetch video stats",
		})
		return
	}

	var statsResult youtubeStatsResponse
	json.Unmarshal(statsData, &statsResult)

	// Step 4: Build stats map
	type statEntry struct {
		ViewCount string
		Duration  string
	}
	statsMap := map[string]statEntry{}
	for _, item := range statsResult.Items {
		statsMap[item.ID] = statEntry{
			ViewCount: item.Statistics.ViewCount,
			Duration:  item.ContentDetails.Duration,
		}
	}

	// Step 5: Filter to real shorts (<=60s) and build response
	videos := []YouTubeVideo{}
	for _, item := range searchResult.Items {
		id := item.ID.VideoID
		stats, ok := statsMap[id]
		if !ok || !isShort(stats.Duration) {
			continue
		}

		videos = append(videos, YouTubeVideo{
			ID:          id,
			Title:       item.Snippet.Title,
			Thumbnail:   item.Snippet.Thumbnails.High.URL,
			ChannelName: item.Snippet.ChannelTitle,
			ViewCount:   stats.ViewCount,
			EmbedURL:    fmt.Sprintf("https://www.youtube.com/embed/%s", id),
		})
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{
		"videos":        videos,
		"nextPageToken": searchResult.NextPageToken,
	})
}

// ─── Helpers ──────────────────────────────────────────────────────────────────

func isShort(duration string) bool {
	var minutes, seconds int
	fmt.Sscanf(duration, "PT%dM%dS", &minutes, &seconds)
	if minutes == 0 {
		fmt.Sscanf(duration, "PT%dS", &seconds)
	}
	total := minutes*60 + seconds
	return total > 0 && total <= 60
}

func fetchJSON(url string) ([]byte, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

func respondJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}