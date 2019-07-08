FROM golang:1.12.6

COPY . /go/src/github.com/jenkins-x/prow-pipeline-controller
WORKDIR /go/src/github.com/jenkins-x/prow-pipeline-controller
RUN make build

FROM scratch

COPY --from=0 /go/src/github.com/jenkins-x/prow-pipeline-controller/build/pipeline /pipeline

CMD ["/pipeline]