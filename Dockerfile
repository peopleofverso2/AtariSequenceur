# ---- build stage -------------------------------------------------
FROM golang:1.23-alpine AS build
WORKDIR /src

# The frontend is embedded via go:embed, so the whole source tree is
# needed before building. go mod tidy resolves dependencies (and writes
# go.sum) inside the build container.
COPY . .
RUN go mod tidy
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /bin/server .

# ---- runtime stage -----------------------------------------------
# distroless/static: no shell, no package manager, runs as non-root.
# The single static binary already contains the frontend.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /bin/server /server
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/server"]
