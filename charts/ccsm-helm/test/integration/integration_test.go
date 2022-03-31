// Copyright 2022 Camunda Services GmbH
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build integration
// +build integration

package integration

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"context"
	"testing"

	"github.com/Nerzal/gocloak/v11"
	"github.com/camunda-cloud/zeebe/clients/go/pkg/pb"
	"github.com/camunda-cloud/zeebe/clients/go/pkg/zbc"
	"github.com/gruntwork-io/terratest/modules/k8s"
	"github.com/gruntwork-io/terratest/modules/logger"
	"github.com/gruntwork-io/terratest/modules/random"
	"github.com/gruntwork-io/terratest/modules/retry"
	"github.com/stretchr/testify/require"
	"github.com/stretchr/testify/suite"
	"google.golang.org/grpc"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type integrationTest struct {
	suite.Suite
	chartPath   string
	release     string
	namespace   string
	kubeOptions *k8s.KubectlOptions
}

func TestIntegration(t *testing.T) {
	chartPath, err := filepath.Abs("../../")
	require.NoError(t, err)

	namespace := "zell-helm-test" // createNamespaceName()
	kubeOptions := k8s.NewKubectlOptions("gke_zeebe-io_europe-west1-b_zeebe-cluster", "", namespace)

	suite.Run(t, &integrationTest{
		chartPath:   chartPath,
		release:     "ccsm-helm-test",
		namespace:   namespace,
		kubeOptions: kubeOptions,
	})
}
//
//func (s *integrationTest) SetupTest() {
//	k8s.CreateNamespace(s.T(), s.kubeOptions, s.namespace)
//}
//
//func (s *integrationTest) TearDownTest() {
//	k8s.DeleteNamespace(s.T(), s.kubeOptions, s.namespace)
//}

func (s *integrationTest) TestServicesEnd2End() {
	// given
	//options := &helm.Options{
	//	KubectlOptions: s.kubeOptions,
	//}

	// when
	//helm.Install(s.T(), options, s.chartPath, s.release)

	// then
	s.awaitCCSMPods()
	_, err := s.loginToIdentity()
	if err != nil {
		s.T().Logf("Error on login %s", err)
	}
	//s.createProcessInstance()
	//
	//s.awaitElasticPods()
	//s.assertProcessDefinitionFromOperate()
	//s.assertTasksFromTasklist()
}

func (s *integrationTest) assertProcessDefinitionFromOperate() {
	message := retry.DoWithRetry(s.T(),
		"Try to query and assert process definition from operate",
		10,
		10*time.Second,
		func() (string, error) {
			responseBuf, err := s.queryProcessDefinitionsFromOperate()
			if err != nil {
				return "", err
			}

			jsonString := responseBuf.String()
			s.T().Logf("Request successful, got as response '%s'", jsonString)
			var objectMap map[string]interface{}
			err = json.Unmarshal(responseBuf.Bytes(), &objectMap)
			if err != nil {
				return "", err
			}

			total := objectMap["total"].(float64)
			s.Require().GreaterOrEqual(total, float64(1))

			s.Require().Contains(jsonString, "it-test-process")

			return "Process definition 'it-test-process' successful queried from operate!", nil
		})
	s.T().Logf(message)
}


func (s *integrationTest) assertTasksFromTasklist() {
	message := retry.DoWithRetry(s.T(),
		"Try to query and assert process definition from operate",
		10,
		10*time.Second,
		func() (string, error) {
			responseBuf, err := s.queryTasksFromTasklist()
			if err != nil {
				return "", err
			}

			jsonString := responseBuf.String()
			s.T().Logf("Request successful, got as response '%s'", jsonString)
			var objectMap map[string]interface{}
			err = json.Unmarshal(responseBuf.Bytes(), &objectMap)
			if err != nil {
				return "", err
			}

			data := objectMap["data"].(map[string]interface{})
			tasks := data["tasks"].([]interface{})
			s.Require().GreaterOrEqual(len(tasks), 1)
			task := tasks[0].(map[string]interface{})
			s.Require().Equal("It Test", task["name"])

			return "User Task 'It Test' successful queried from tasklist!", nil
		})
	s.T().Logf(message)
}


func (s *integrationTest) queryTasksFromTasklist() (*bytes.Buffer, error) {
	operateServiceName := fmt.Sprintf("%s-tasklist", s.release)
	endpoint, closeFn := s.createPortForwardedHttpClient(operateServiceName)
	defer closeFn()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	httpClient := http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}

	err = s.loginOnService(endpoint, httpClient)
	if err != nil {
		return nil, err
	}

	// curl -i -H "Content-Type: application/json" -XPOST "http://localhost:8080/graphql" --cookie "ope-session"  -d '{"query": "{tasks(query:{}){name}}"}'
	return s.queryApi(httpClient, "http://"+endpoint+"/graphql", bytes.NewBufferString(`{"query": "{tasks(query:{}){name}}"}`))
}

func (s *integrationTest) createProcessInstance() {
	serviceName := fmt.Sprintf("%s-zeebe-gateway", s.release)
	client, closeFn, err := s.createPortForwardedClient(serviceName)
	s.Require().NoError(err, "failed to create Zeebe client")
	defer closeFn()

	s.assertGatewayTopology(err, client)
	deployProcessResponse := s.deployProcess(err, client)
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()

	message := retry.DoWithRetry(s.T(), "Try to create Process instance", 10, 1*time.Second, func() (string, error) {
		_, err = client.NewCreateInstanceCommand().ProcessDefinitionKey(deployProcessResponse.Processes[0].ProcessDefinitionKey).Send(ctx)
		return "Process instance created.", err
	})
	s.T().Logf(message)
}

func (s *integrationTest) loginToIdentity() (*bytes.Buffer, error) {

	secret := k8s.GetSecret(s.T(), s.kubeOptions, "ccsm-helm-test-keycloak")
	password := secret.Data["admin-password"]

	keycloakServiceName := fmt.Sprintf("%s-keycl", s.release)
	_, closePortForward := s.createPortForwardedHttpClientWithPort(keycloakServiceName, 18080)
	defer closePortForward()

	client := gocloak.NewClient("http://localhost:18080")
	restyClient := client.RestyClient()
	restyClient.SetDebug(true)
	restyClient.SetTLSClientConfig(&tls.Config{ InsecureSkipVerify: true })
	ctx := context.Background()
	// client.Login(ctx, "camunda-identity", "", "camunda-platform", "demo", "demo")
	token, err :=  client.LoginAdmin(ctx, "admin", string(password), "master")
	if err != nil {
		return nil, err
	}

	s.T().Logf("%s", token.AccessToken)

	//client.GetClientSecret()


	otherClients, err := client.GetClients(ctx, token.AccessToken, "camunda-platform", gocloak.GetClientsParams{})// client.GetClient(ctx, token.AccessToken, "camunda-platform", "camunda-identity")
	if (err != nil) {
		return nil, err
	}

	for _, returnedClient := range otherClients {
		if (returnedClient != nil) {
			if (*returnedClient.ClientID == "camunda-identity") || strings.Contains(*returnedClient.ClientID, "operate") {
				id := *returnedClient.ID

				getClient, err := client.GetClient(ctx, token.AccessToken, "camunda-platform", id)
				if (err != nil) {
					return nil, err
				}
				if (getClient.Secret != nil)				{
					s.T().Logf("%s", *getClient.Secret)
				}


				s.T().Logf("%v", getClient)
			}
		}

	}

	identityServiceName := fmt.Sprintf("%s-identity", s.release)
	endpoint, closeFn := s.createPortForwardedHttpClientWithPort(identityServiceName, 8080)
	defer closeFn()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	httpClient := http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}

	request, err := http.NewRequest("GET", "http://" + endpoint + "/auth/login", nil)
	if err != nil {
		return nil, err
	}
	request.Close = true
	response, err := httpClient.Do(request)
	if err != nil {
		return nil,err
	}
	defer response.Body.Close()
	body := response.Body
	b, err := io.ReadAll(body)

	//  ... get body from /auth/login redirect
	// <form id="kc-form-login" onsubmit="login.disabled = true; return true;"
	//		action="http://localhost:18080/auth/realms/camunda-platform/login-actions/authenticate?session_code=B0BxW2ST2DH0NYE1J-THQncuCVc2yPck5JFmgEnLWbM&amp;execution=be1c2750-2b28-4044-8cf3-22b1331efeae&amp;client_id=camunda-identity&amp;tab_id=tp2zBJnsh6o"
	//		method="post">

	regexCompiled := regexp.MustCompile("(action=\")(.*)(\"[\\s\\w]+=\")")

	submatch := regexCompiled.FindStringSubmatch(string(b))

	//var res map[string]interface{}
	sessionUrl := string(submatch[2])
	sessionUrl = strings.Replace(sessionUrl, "&amp;", "&", -1)
	values := url.Values{
		"username":  {"demo"},
		"password":  {"demo"},
		"credentialId": {""},
	}


	resp, err := httpClient.PostForm(sessionUrl, values)
	if err != nil {
		return nil, err
	}

	b, err = io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	fmt.Println(string(b))

	// curl -i -H "Content-Type: application/json" -XPOST "http://localhost:8080/v1/process-definitions/list" --cookie "ope-session" -d "{}"
	return nil, nil
}

func (s *integrationTest) printBody(err error, b []byte) (error) {
	// b, err := ioutil.ReadAll(resp.Body)  Go.1.15 and earlier
	if err != nil {
		s.T().Logf("FAIILED")
		return err
	}
	s.T().Logf("Response Body: \n%s", string(b))
	return  nil
}

func (s *integrationTest) queryProcessDefinitionsFromOperate() (*bytes.Buffer, error) {
	operateServiceName := fmt.Sprintf("%s-operate", s.release)
	endpoint, closeFn := s.createPortForwardedHttpClient(operateServiceName)
	defer closeFn()

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}

	httpClient := http.Client{
		Jar:     jar,
		Timeout: 30 * time.Second,
	}

	err = s.loginOnService(endpoint, httpClient)
	if err != nil {
		return nil, err
	}

//http://localhost:8081/api/clients

	// curl -i -H "Content-Type: application/json" -XPOST "http://localhost:8080/v1/process-definitions/list" --cookie "ope-session" -d "{}"
	return s.queryApi(httpClient, "http://"+endpoint+"/v1/process-definitions/list", bytes.NewBufferString("{}"))
}

func (s *integrationTest) queryApi(httpClient http.Client, url string, jsonData *bytes.Buffer) (*bytes.Buffer, error) {
	response, err := httpClient.Post(url, "application/json", jsonData)
	if err != nil {
		return nil, err
	}
	s.Require().Equal(200, response.StatusCode)

	buf := new(bytes.Buffer)
	_, err = buf.ReadFrom(response.Body)
	defer response.Body.Close()
	if err != nil {
		return nil, err
	}
	return buf, nil
}

func (s *integrationTest) loginOnService(endpoint string, httpClient http.Client) error {
	// curl --include --request POST --cookie-jar "ope-session" "http://localhost:8080/api/login?username=demo&password=demo"
	request, err := http.NewRequest("POST", "http://"+endpoint+"/api/login?username=demo&password=demo", nil)
	if err != nil {
		return err
	}
	request.Close = true

	_, err = httpClient.Do(request)
	if err != nil {
		return err
	}
	return nil
}

func (s *integrationTest) awaitCCSMPods() {
	// await that all ccsm related pods become ready
	pods := k8s.ListPods(s.T(), s.kubeOptions, v1.ListOptions{LabelSelector: "app=camunda-cloud-self-managed"})

	for _, pod := range pods {
		k8s.WaitUntilPodAvailable(s.T(), s.kubeOptions, pod.Name, 1000, 1*time.Second)
	}
}

func (s *integrationTest) awaitElasticPods() {
	// await that all elastic related pods become ready, otherwise operate and tasklist can't answer requests
	pods := k8s.ListPods(s.T(), s.kubeOptions, v1.ListOptions{LabelSelector: "app=elasticsearch-master"})

	for _, pod := range pods {
		k8s.WaitUntilPodAvailable(s.T(), s.kubeOptions, pod.Name, 10, 10*time.Second)
	}
}

func (s *integrationTest) deployProcess(err error, client zbc.Client) *pb.DeployProcessResponse {
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()
	deployProcessResponse, err := client.NewDeployProcessCommand().AddResourceFile("it-test-process.bpmn").Send(ctx)
	s.Require().NoError(err, "failed to deploy process model")
	s.Require().Equal(1, len(deployProcessResponse.Processes))
	return deployProcessResponse
}

func (s *integrationTest) assertGatewayTopology(err error, client zbc.Client) {
	ctx, cancelFn := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelFn()
	topology, err := client.NewTopologyCommand().Send(ctx)

	s.Require().NoError(err, "failed to obtain gateway topology")
	s.Require().EqualValues(3, topology.ClusterSize)
	s.Require().EqualValues(3, topology.PartitionsCount)
	s.Require().EqualValues(3, topology.ReplicationFactor)
}

func (s *integrationTest) createPortForwardedClient(serviceName string) (zbc.Client, func(), error) {
	// NOTE: this only waits until the service is created, not until the underlying pods are ready to receive traffic
	k8s.WaitUntilServiceAvailable(s.T(), s.kubeOptions, serviceName, 90, 1*time.Second)

	// port forward the gateway service to avoid having to set up a public endpoint that the test can access externally
	localGatewayPort := k8s.GetAvailablePort(s.T())
	tunnel := k8s.NewTunnel(s.kubeOptions, k8s.ResourceTypeService, serviceName, localGatewayPort, 26500)

	// the gateway is not ready/receiving traffic until at least one leader is present
	s.waitUntilPortForwarded(tunnel, 30, 2*time.Second)

	endpoint := fmt.Sprintf("localhost:%d", localGatewayPort)
	client, err := zbc.NewClient(&zbc.ClientConfig{
		GatewayAddress:         endpoint,
		DialOpts:               []grpc.DialOption{},
		UsePlaintextConnection: true,
	})
	if err != nil {
		return nil, tunnel.Close, err
	}

	return client, func() { client.Close(); tunnel.Close() }, nil
}

func (s *integrationTest) createPortForwardedHttpClientWithPort(serviceName string, port int) (string, func()) {
	// NOTE: this only waits until the service is created, not until the underlying pods are ready to receive traffic
	k8s.WaitUntilServiceAvailable(s.T(), s.kubeOptions, serviceName, 90, 1*time.Second)

	// remote port needs to be container port - not service port!
	tunnel := k8s.NewTunnel(s.kubeOptions, k8s.ResourceTypeService, serviceName, port, 8080)

	// the gateway is not ready/receiving traffic until at least one leader is present
	s.waitUntilPortForwarded(tunnel, 30, 2*time.Second)

	endpoint := fmt.Sprintf("localhost:%d", port)
	return endpoint, func() {
		tunnel.Close()
	}
}

func (s *integrationTest) createPortForwardedHttpClient(serviceName string) (string, func()) {
	// NOTE: this only waits until the service is created, not until the underlying pods are ready to receive traffic
	k8s.WaitUntilServiceAvailable(s.T(), s.kubeOptions, serviceName, 90, 1*time.Second)

	// port forward the service to avoid having to set up a public endpoint that the test can access externally
	localPort := k8s.GetAvailablePort(s.T())
	// remote port needs to be container port - not service port!
	tunnel := k8s.NewTunnel(s.kubeOptions, k8s.ResourceTypeService, serviceName, localPort, 8080)

	// the gateway is not ready/receiving traffic until at least one leader is present
	s.waitUntilPortForwarded(tunnel, 30, 2*time.Second)

	endpoint := fmt.Sprintf("localhost:%d", localPort)
	return endpoint, func() {
		tunnel.Close()
	}
}

func (s *integrationTest) waitUntilPortForwarded(tunnel *k8s.Tunnel, retries int, sleepBetweenRetries time.Duration) {
	statusMsg := fmt.Sprintf("Waiting to port forward for endpoint %s", tunnel.Endpoint())
	message := retry.DoWithRetry(
		s.T(),
		statusMsg,
		retries,
		sleepBetweenRetries,
		func() (string, error) {
			err := tunnel.ForwardPortE(s.T())
			if err != nil {
				return "", err
			}

			return fmt.Sprintf("Endpoint %s is now forwarded", tunnel.Endpoint()), nil
		},
	)
	logger.Logf(s.T(), message)
}

func truncateString(str string, num int) string {
	shortenStr := str
	if len(str) > num {
		shortenStr = str[0:num]
	}
	return shortenStr
}

func createNamespaceName() string {
	// if triggered by a github action the environment variable is set
	// we use it to better identify the test
	commitSHA, exist := os.LookupEnv("GITHUB_SHA")
	namespace := "ccsm-helm-"
	if !exist {
		namespace += strings.ToLower(random.UniqueId())
	} else {
		namespace += commitSHA
	}

	// max namespace length is 63 characters
	// https://kubernetes.io/docs/concepts/overview/working-with-objects/names/#dns-label-names
	return truncateString(namespace, 63)
}
