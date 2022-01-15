
FROM golang:1.16 as builder

WORKDIR /app
COPY go.mod .
COPY go.sum .
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o podium .

FROM alpine:3.9

WORKDIR /app
COPY --from=builder /app/podium podium
RUN chmod +x podium

EXPOSE 8880
EXPOSE 8881

COPY config/default.yaml config/default.yaml

ENV PODIUM_REDIS_HOST localhost
ENV PODIUM_REDIS_PORT 6379
ENV PODIUM_REDIS_PASSWORD ""
ENV PODIUM_REDIS_DB 0
ENV PODIUM_SENTRY_URL ""
ENV PODIUM_BASICAUTH_USERNAME ""
ENV PODIUM_BASICAUTH_PASSWORD ""
ENV PODIUM_NOTSECURE_GET_REQUEST "true"

ENTRYPOINT ["/app/podium"]
CMD ["start", "-c", "/app/config/default.yaml", "-p", "8880", "-g" ,"8881"]
