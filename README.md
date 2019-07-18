# Prow Pipeline Controller

This is a replacement of the original Prow [pipeline controller](https://github.com/kubernetes/test-infra/tree/master/prow/cmd/pipeline) intended for use within Jenkins-X.

## Development

The following paragraphs describe how to build and work with the source of this application.

### Prerequisites

The project is written in [Go](https://golang.org/), so you will need a working Go installation (Go version >= 1.12.4).

The build itself is driven by GNU [Make](https://www.gnu.org/software/make/) which also needs to be installed on your system.

### Compile the code

```bash
$ make `uname | tr '[:upper:]' '[:lower:]'`
```

After successful compilation the `pipeline` binary can be found in the `bin` directory.

### Run the tests

```bash   
$ make test
```

### Check formatting

```bash   
$ make check
```

### Build Docker image

```bash   
$ make docker
```

### Cleanup

```bash   
$ make clean
```