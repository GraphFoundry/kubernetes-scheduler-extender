FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY internal ./internal
COPY cmd ./cmd
RUN go build -o /out/extender ./cmd/app

FROM alpine:3.20
WORKDIR /app
COPY --from=build /out/extender /app/extender
EXPOSE 9000
ENV PORT=9000
ENTRYPOINT ["/app/extender"]
