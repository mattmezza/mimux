# Stage 1: Build Tailwind CSS
FROM node:22-alpine AS css
WORKDIR /build
COPY package.json package-lock.json ./
RUN npm ci
COPY web/static/css/ web/static/css/
COPY web/static/js/app.js web/static/js/app.js
COPY web/templates/ web/templates/
RUN npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --minify

# Stage 2: Build Go binary
FROM golang:1.25-alpine AS go
ARG VERSION=dev
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /build/web/static/css/dist.css web/static/css/dist.css
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X main.version=${VERSION}" -o sm ./cmd/sm

# Stage 3: Final image
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go /build/sm /usr/local/bin/sm
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["sm"]
