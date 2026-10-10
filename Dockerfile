# syntax=docker/dockerfile:1

FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod go mod download
COPY . .
RUN --mount=type=cache,id=gomod,target=/go/pkg/mod --mount=type=cache,id=gobuild,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/ ./cmd/api ./cmd/migrate

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /out/migrate /
USER nonroot:nonroot
ENV PORT=8080
EXPOSE 8080
# Run migrations as a release step with: docker run --entrypoint /migrate <image> up
ENTRYPOINT ["/api"]
