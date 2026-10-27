FROM node:22-alpine@sha256:fe4f6d74e20370acb04ad889077d14d0f64a9c8ea6a42bc60cddf1e844699914 AS web
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --ignore-scripts
COPY web/ ./
RUN npm run build

FROM golang:1.26@sha256:3745f1aa11822297b79113f5e84ce2bffeb039734571c39855809cfe9e5d7be6 AS build
WORKDIR /src
COPY api/go.mod api/go.sum ./
RUN go mod download
COPY api/ ./
COPY --from=web /web/dist/ internal/webui/dist/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/portal-api .

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=build /out/portal-api /portal-api
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/portal-api"]
