# ---- build stage -------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src

# Resolve and cache dependencies first so source edits don't bust the
# module download layer. go.sum is committed for reproducible builds.
COPY go.mod go.sum ./
RUN go mod download

# The frontend is embedded via go:embed, so the whole source tree is
# needed before building the binary.
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/server .

# ---- runtime stage -----------------------------------------------
# distroless/static: no shell, no package manager, runs as non-root.
# The single static binary already contains the frontend.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
