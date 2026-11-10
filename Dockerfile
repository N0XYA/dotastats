# syntax=docker/dockerfile:1

FROM golang:1.27.3 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" \
    -o /out/dotastats .

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/dotastats /dotastats

ENTRYPOINT ["/dotastats"]
CMD ["serve", "--no-browser", "--addr", "127.0.0.1:8000", "--data-dir", "/data"]
