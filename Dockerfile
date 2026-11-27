FROM docker.io/library/golang:1.27-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/krakozhia .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/krakozhia /krakozhia
EXPOSE 8080
ENTRYPOINT ["/krakozhia"]
CMD ["serve"]
