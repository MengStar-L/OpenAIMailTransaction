FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/atelier .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates tzdata && addgroup -S atelier && adduser -S -G atelier atelier && mkdir /data && chown atelier:atelier /data
COPY --from=build /out/atelier /usr/local/bin/atelier
USER atelier
WORKDIR /app
ENV LISTEN_ADDR=:8080 DATA_DIR=/data
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["atelier"]
