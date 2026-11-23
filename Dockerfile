# salottod: the bouncer daemon plus the two renderers (salotto-web, salotto-ssh).
# One image, three binaries, because they share internal/ui and would drift apart
# if versioned separately.
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/salottod ./cmd/salottod \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/salotto-web ./cmd/salotto-web \
 && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o /out/salotto-ssh ./cmd/salotto-ssh

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/salottod /usr/local/bin/salottod
COPY --from=build /out/salotto-web /usr/local/bin/salotto-web
COPY --from=build /out/salotto-ssh /usr/local/bin/salotto-ssh
# 6667 the client listener, 6668 stats, 113 ident, 8080 web, 2222 ssh.
EXPOSE 6667 6668 113 8080 2222
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/salottod"]
CMD ["-config", "/etc/salotto/salotto.toml"]
