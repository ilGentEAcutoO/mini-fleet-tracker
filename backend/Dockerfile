# syntax=docker/dockerfile:1.7

# ---------- build stage ----------
FROM golang:1.23-alpine AS build

ARG GIT_COMMIT=dev
ENV CGO_ENABLED=0 \
    GOOS=linux

WORKDIR /src

# Cache module downloads in a separate layer.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go build \
      -trimpath \
      -ldflags="-s -w -X main.gitCommit=${GIT_COMMIT}" \
      -o /out/api \
      ./cmd/api

# ---------- runtime stage ----------
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /
COPY --from=build /out/api /api

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
