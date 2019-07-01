module github.com/jenkins-x/prow-pipeline-controller

go 1.12

require (
	github.com/knative/pkg v0.0.0-20190627143708-1864f499dcaa
	github.com/sirupsen/logrus v1.4.2
	github.com/tektoncd/pipeline v0.4.0
	k8s.io/api v0.0.0-20190627205229-acea843d18eb
	k8s.io/apimachinery v0.0.0-20190629125103-05b5762916b3
	k8s.io/client-go v11.0.0+incompatible
	k8s.io/test-infra v0.0.0-20190628235729-eb4a109306ac
	k8s.io/utils v0.0.0-20190607212802-c55fbcfc754a // indirect
	knative.dev/pkg v0.0.0-20190627143708-1864f499dcaa // indirect
)
