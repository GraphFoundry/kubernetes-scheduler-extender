FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY main.go ./
RUN go build -o /out/scheduler main.go

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/scheduler /app/scheduler
# Scheduler loop does not expose a port
ENTRYPOINT ["/app/scheduler"]
