FROM alpine:3.10

RUN apk --update add ca-certificates git

COPY ./build/pipeline /usr/bin/pipeline

ENTRYPOINT ["/usr/bin/pipeline"]