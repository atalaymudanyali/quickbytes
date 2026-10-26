# One recipe for every Go service: SERVICE picks which command in cmd/ to build
# (api by default; docker-compose.yml passes payprovider for the payment provider).
ARG SERVICE=api

# Build stage: compile a static binary with the full Go toolchain.
FROM golang:1.27-alpine AS build
ARG SERVICE
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/service ./cmd/${SERVICE}

# Run stage: only the binary, on a minimal image with no shell, running as non-root.
FROM gcr.io/distroless/static-debian12:nonroot
# No EXPOSE: the API listens on 8080 and the provider on 8090 (see docker-compose.yml).
COPY --from=build /out/service /service
ENTRYPOINT ["/service"]
