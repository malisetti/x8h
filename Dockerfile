FROM golang:1.11.4-stretch as base
WORKDIR /usr/src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -o HN-Posts-last-8hrs

FROM scratch
COPY --from=base /usr/src/HN-Posts-last-8hrs /HN-Posts-last-8hrs
COPY --from=base /usr/src/index.html /index.html
EXPOSE 8080
CMD ["/HN-Posts-last-8hrs"]
