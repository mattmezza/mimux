# The licence shop. Built and run only on the maintainer's VPS; it is not part
# of any mimux release. Build context is this directory (account/ is its own Go
# module and imports nothing from the parent one).
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-s -w" -o account .

# ca-certificates is not optional: the Stripe API calls fail without it.
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /build/account /usr/local/bin/account
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["account"]
