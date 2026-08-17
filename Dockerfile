# Stage 1: Build Tailwind CSS. Pinned to the build platform (native) so multi-
# arch builds don't run npm under QEMU emulation.
FROM --platform=$BUILDPLATFORM node:22-alpine AS css
WORKDIR /build
COPY package.json package-lock.json ./
RUN npm ci
COPY web/static/css/ web/static/css/
COPY web/static/js/app.js web/static/js/app.js
COPY web/templates/ web/templates/
RUN npx @tailwindcss/cli -i web/static/css/app.css -o web/static/css/dist.css --minify

# Stage 2: Build the Go binary. Runs on the native build platform and
# cross-compiles to the target arch (CGO disabled), so arm64 images build at
# native speed instead of emulating the whole compile.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS go
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=css /build/web/static/css/dist.css web/static/css/dist.css
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w -X main.version=${VERSION}" -o mimux ./cmd/mimux

# Stage 3: Final image (per target arch — just ca-certs + the binary).
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=go /build/mimux /usr/local/bin/mimux
VOLUME /data
EXPOSE 8083
ENTRYPOINT ["mimux"]
