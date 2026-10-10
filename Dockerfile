FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/server /server
USER 65532:65532
ENV HTTP_ADDR=0.0.0.0:8080 METRICS_ADDR=127.0.0.1:9090 MODEL_CONFIG_PATH=/config/models.json
EXPOSE 8080
ENTRYPOINT ["/server"]
