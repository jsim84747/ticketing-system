FROM golang:1.27.2-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /server ./cmd/server
FROM alpine:3.22
RUN adduser -D -u 10001 app
USER app
COPY --from=build /server /server
EXPOSE 8080
ENTRYPOINT ["/server"]
