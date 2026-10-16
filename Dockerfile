FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM alpine:3.22
RUN adduser -D -u 10001 app && mkdir -p /var/lib/dellos && chown app /var/lib/dellos
COPY --from=build /out/server /usr/local/bin/server
COPY config.json /etc/dellos/config.json
USER app
ENV CONFIG_PATH=/etc/dellos/config.json
EXPOSE 8080
ENTRYPOINT ["server"]
