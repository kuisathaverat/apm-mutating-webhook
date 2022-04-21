package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
)

func main() {
	var (
		runtimeScheme = runtime.NewScheme()
		codecs        = serializer.NewCodecFactory(runtimeScheme)
		deserializer  = codecs.UniversalDeserializer()
		certPath      = flag.String("certFile", "/opt/webhook/certs/cert.pem", "path to cert.pem")
		keyPath       = flag.String("keyFile", "/opt/webhook/certs/key.pem", "path to key.pem")
	)
	flag.Parse()
	ss := &server{
		d: deserializer,
		l: log.Default(),
		c: map[string]agentConfig{
			"java": agentConfig{
				Image: "docker.elastic.co/observability/apm-agent-java:1.23.0",
				Environment: map[string]string{
					"ELASTIC_APM_SERVER_URLS":                      "http://34.78.173.219:8200",
					"ELASTIC_APM_SERVICE_NAME":                     "petclinic",
					"ELASTIC_APM_ENVIRONMENT":                      "test",
					"ELASTIC_APM_LOG_LEVEL":                        "debug",
					"ELASTIC_APM_PROFILING_INFERRED_SPANS_ENABLED": "true",
					"JAVA_TOOL_OPTIONS":                            "-javaagent:/elastic/apm/agent/elastic-apm-agent.jar",
				},
			},
		},
	}
	_ = ss
	s := &http.Server{
		Addr:    ":8443",
		Handler: ss,
	}
	log.Println("listening on :8443")
	log.Fatal(s.ListenAndServeTLS(*certPath, *keyPath))
}

type server struct {
	d runtime.Decoder
	l *log.Logger
	c map[string]agentConfig
}

type config struct {
	Agents map[string]agentConfig `yaml:"agents"`
}

type agentConfig struct {
	Image       string            `yaml:"image"`
	Environment map[string]string `yaml:"environment"`
}

const apmAnnotation = "elastic-apm-agent"

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		sendError(err, w)
		return
	}

	admReview := admissionv1.AdmissionReview{}
	if err := json.Unmarshal(body, &admReview); err != nil {
		sendError(err, w)
		return
	}

	if err := s.mutate(&admReview); err != nil {
		sendError(err, w)
		return
	}

	resp, err := json.Marshal(admReview)
	if err != nil {
		sendError(err, w)
		return
	}
	w.Write(resp)
}

func sendError(err error, w http.ResponseWriter) {
	log.Println(err)
	w.WriteHeader(http.StatusInternalServerError)
	fmt.Fprintf(w, "%s", err)
}

// TODO:
// - check for annotation
// - apply correct environment variables based on annotation value
func (s *server) mutate(admReview *admissionv1.AdmissionReview) error {
	var pod *corev1.Pod

	ar := admReview.Request
	resp := admissionv1.AdmissionResponse{}

	if ar == nil {
		// TODO: Is this right?
		return nil
	}

	if err := json.Unmarshal(ar.Object.Raw, &pod); err != nil {
		return fmt.Errorf("unable unmarshal pod json object %v", err)
	}

	resp.Allowed = true
	resp.UID = ar.UID

	// TODO: encapsulate this whole config logic into a fn
	result := new(metav1.Status)
	annotations := pod.ObjectMeta.GetAnnotations()
	if annotations == nil {
		result.Message = "no annotations present"
		resp.Result = result
		admReview.Response = &resp
		return nil
	}
	// TODO: Do we want to support multiple comma-separated agents?
	agent, ok := annotations[apmAnnotation]
	if !ok {
		result.Message = "missing annotation `elastic-apm-agent`"
		resp.Result = result
		admReview.Response = &resp
		return nil
	}
	// TODO: validate the config has a container field
	config, ok := s.c[agent]
	if !ok {
		result.Message = fmt.Sprintf("no config for agent `%s`", agent)
		resp.Result = result
		admReview.Response = &resp
		return nil
	}

	pT := admissionv1.PatchTypeJSONPatch
	resp.PatchType = &pT

	patch := createPatch(config, pod.Spec)

	marshaled, err := json.Marshal(patch)
	if err != nil {
		return err
	}
	resp.Patch = marshaled

	resp.Result = &metav1.Status{
		Status: "Success",
	}

	admReview.Response = &resp
	return nil
}
