FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY main.go ./
RUN go build -o /out/best-node-selector main.go

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/best-node-selector /app/best-node-selector
EXPOSE 9000
ENV PORT=9000
ENTRYPOINT ["/app/best-node-selector"]
