# syntax=docker/dockerfile:1.7

# ---------- build stage ----------
FROM golang:1.25-alpine AS build

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
#
# HEALTHCHECK directive intentionally omitted (TASK-065).
#
# Rationale:
#   * Deploy target is Cloudflare Containers, which runs its own liveness
#     probe independent of Docker HEALTHCHECK (see the container worker at
#     workers/api/src/index.ts — the @cloudflare/containers base handles
#     wake-on-request + sleep with no Dockerfile-side cooperation).
#   * The runtime image is gcr.io/distroless/static-debian12:nonroot, which
#     ships no shell, no curl, and no wget. The only ways to add a probe
#     are (a) compile a second static Go binary that hits /healthz and
#     COPY it in alongside /api, or (b) switch to a heavier base such as
#     distroless/static-debian12:debug (busybox shell + wget). Both add
#     attack surface or build complexity for zero production benefit.
#   * For local Docker testing the operator can run the probe from a
#     sibling container or the host:
#         docker run --rm --network=host curlimages/curl:latest \
#             -fsS http://localhost:8080/healthz
#     The /healthz endpoint is the same one CF Containers polls.
#
# If this assumption ever changes (e.g. self-hosting on plain Docker
# Swarm / k8s without an external probe), the cheapest fix is to add a
# tiny `./cmd/healthcheck` package that does `http.Get("/healthz")` with
# a 2s timeout, build it in the same multi-stage build, and add:
#     HEALTHCHECK --interval=10s --timeout=2s --retries=3 \
#         CMD ["/healthcheck"]
#
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

WORKDIR /
COPY --from=build /out/api /api

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/api"]
