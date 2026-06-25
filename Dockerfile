FROM golang:1.22-alpine AS builder
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN go build -o /out/model-monitor-lite .

FROM alpine:3.20
WORKDIR /app
COPY --from=builder /out/model-monitor-lite /app/model-monitor-lite
COPY static /app/static
ENV SERVER_HOST=0.0.0.0
ENV SERVER_PORT=1145
EXPOSE 1145
CMD ["/app/model-monitor-lite"]
