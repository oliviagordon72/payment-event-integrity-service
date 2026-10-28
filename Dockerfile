FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /payments ./cmd/payments

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /payments /payments
EXPOSE 8080
ENTRYPOINT ["/payments"]
CMD ["serve"]
