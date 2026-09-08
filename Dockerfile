FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/k8s-agent .

FROM gcr.io/distroless/static-debian12
COPY --from=build /out/k8s-agent /k8s-agent
USER nonroot:nonroot
ENTRYPOINT ["/k8s-agent"]
