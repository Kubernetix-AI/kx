# Written by kx deploy for a Go app.
# The build runs on this computer's own platform and compiles for the image's.
FROM --platform=${BUILDPLATFORM:-linux/amd64} golang:1.23-alpine AS build
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -buildvcs=false -trimpath -o /out/app ./cmd/server

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
ENV PORT=8080
EXPOSE 8080
# Some hosts refuse containers that run as root, and can only check a numeric user.
USER 65532:65532
CMD ["/app"]
