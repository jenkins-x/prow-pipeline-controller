module github.com/jenkins-x/prow-pipeline-controller

go 1.12

require (
	github.com/evanphx/json-patch v4.2.0+incompatible
	github.com/knative/pkg v0.0.0-20190330034653-916205998db9
	github.com/sirupsen/logrus v1.4.2
	github.com/tektoncd/pipeline v0.1.1-0.20190327171839-7c43fbae2816
	k8s.io/api v0.0.0-20181128191700-6db15a15d2d3
	k8s.io/apiextensions-apiserver v0.0.0-20181128195303-1f84094d7e8e
	k8s.io/apimachinery v0.0.0-20190122181752-bebe27e40fb7
	k8s.io/client-go v9.0.0+incompatible
	k8s.io/test-infra v0.0.0-20190709111002-c9d9081e68ff
)
