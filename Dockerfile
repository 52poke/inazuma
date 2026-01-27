FROM golang:1.25-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o /out/inazuma ./cmd/inazuma

FROM alpine:3.23
RUN apk add --no-cache ca-certificates
COPY --from=build /out/inazuma /usr/local/bin/inazuma
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/inazuma"]
