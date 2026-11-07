# syntax=docker/dockerfile:1

FROM golang:1.27-alpine AS build

ARG VERSION=dev

WORKDIR /src

# Cache dependency downloads separately from the source.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/agent-task-runner ./cmd/agent-task-runner

FROM gcr.io/distroless/static:nonroot

ARG VERSION=dev
LABEL org.opencontainers.image.title="agent-task-runner" \
      org.opencontainers.image.version="${VERSION}"

COPY --from=build /out/agent-task-runner /agent-task-runner

# Match the runAsUser used by the Deployment and by the worker pods.
USER 10000:10000
EXPOSE 8080

ENTRYPOINT ["/agent-task-runner"]
