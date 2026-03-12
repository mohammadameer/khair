FROM golang:1.24-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -o /out/good-videos .

FROM alpine:3.21

RUN apk add --no-cache ca-certificates && adduser -D appuser

WORKDIR /home/appuser
USER appuser

COPY --from=build /out/good-videos ./good-videos

ENV PORT=8080
EXPOSE 8080

CMD ["./good-videos"]
