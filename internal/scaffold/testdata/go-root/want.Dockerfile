# Written by kx deploy for a Go app.
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -o /out/app .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENV PORT=8080
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 65532:65532
CMD ["/app"]
