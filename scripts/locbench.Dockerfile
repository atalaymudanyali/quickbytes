# The location benchmark, on an image with a shell tool the services' images leave
# out: `tc`, which it uses to make its own network bad on purpose. It runs inside
# Docker's network, next to the service, because Docker's port forwarding on a Mac
# would be part of what's measured otherwise.
FROM golang:1.27-alpine
RUN apk add --no-cache iproute2
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /usr/local/bin/locbench ./cmd/locbench
ENTRYPOINT ["locbench"]
