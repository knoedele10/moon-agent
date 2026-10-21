FROM golang:1.25.3-alpine AS build

WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/blog-my ./cmd/blog-my

FROM alpine:3.22
RUN apk add --no-cache ca-certificates \
    && addgroup -S app && adduser -S -G app app
WORKDIR /app
COPY --from=build /out/blog-my /app/blog-my
COPY templates /app/templates
COPY skills /app/skills
ENV APP_HOST=0.0.0.0 APP_PORT=8080 TEMPLATES_DIR=/app/templates
EXPOSE 8080
USER app
ENTRYPOINT ["/app/blog-my"]
