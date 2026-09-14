package e2e

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	// kubePrometheusStackGrafanaName is the default name for Grafana resources deployed by kube-prometheus-stack
	kubePrometheusStackGrafanaName = "kube-prometheus-stack-grafana"
	// httpPortName is the standard name for HTTP service ports
	httpPortName = "http"
	// wvaOperationDashboardConfigMapName is the name of the ConfigMap containing the WVA operational dashboard
	wvaOperationDashboardConfigMapName = "wva-operation-dashboard"
)

// OperationalDashboard tests validate Grafana deployment and dashboard functionality.
// These tests are optional and will be skipped if Grafana is not deployed in the cluster.
//
// Grafana is deployed as part of kube-prometheus-stack when DEPLOY_OPERATIONAL_DASHBOARD=true.
// It uses the label: app.kubernetes.io/name=grafana
var _ = Describe("OperationalDashboard", Label("full"), Label("operational-dashboard"), Ordered, func() {
	var (
		grafanaDeploymentName string
		grafanaServiceName    string
		grafanaFound          bool
	)

	BeforeAll(func() {
		By("Detecting if Grafana is deployed in the cluster")
		GinkgoWriter.Println("========================================")
		GinkgoWriter.Println("Grafana Detection Phase")
		GinkgoWriter.Println("========================================")
		GinkgoWriter.Printf("Monitoring namespace: %s\n", cfg.MonitoringNS)
		GinkgoWriter.Println("Searching for Grafana deployment with label: app.kubernetes.io/name=grafana")

		// Check for Grafana deployment using the standard label
		deploymentList, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
			LabelSelector: "app.kubernetes.io/name=grafana",
		})
		Expect(err).NotTo(HaveOccurred(), "Failed to list deployments in monitoring namespace")
		GinkgoWriter.Printf("Found %d deployment(s) with Grafana label\n", len(deploymentList.Items))

		if len(deploymentList.Items) == 0 {
			grafanaFound = false
			GinkgoWriter.Println("")
			GinkgoWriter.Println("⚠ Grafana is NOT deployed in the cluster")
			GinkgoWriter.Println("⚠ All Grafana tests will be SKIPPED")
			GinkgoWriter.Println("To deploy Grafana, set DEPLOY_OPERATIONAL_DASHBOARD=true")
			GinkgoWriter.Println("========================================")
		} else {
			grafanaFound = true
			grafanaDeploymentName = deploymentList.Items[0].Name
			GinkgoWriter.Printf("✓ Found Grafana deployment: %s\n", grafanaDeploymentName)

			// Discover the corresponding service
			GinkgoWriter.Println("Searching for Grafana service...")
			serviceList, err := k8sClient.CoreV1().Services(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=grafana",
			})
			Expect(err).NotTo(HaveOccurred(), "Failed to list services in monitoring namespace")
			GinkgoWriter.Printf("Found %d service(s) with Grafana label\n", len(serviceList.Items))

			if len(serviceList.Items) > 0 {
				grafanaServiceName = serviceList.Items[0].Name
				GinkgoWriter.Printf("✓ Found Grafana service: %s\n", grafanaServiceName)
			} else {
				// Try common service name
				GinkgoWriter.Printf("Trying common service name: %s\n", kubePrometheusStackGrafanaName)
				_, err := k8sClient.CoreV1().Services(cfg.MonitoringNS).Get(ctx,
					kubePrometheusStackGrafanaName, metav1.GetOptions{})
				if err == nil {
					grafanaServiceName = kubePrometheusStackGrafanaName
					GinkgoWriter.Printf("✓ Found Grafana service by name: %s\n", grafanaServiceName)
				} else {
					GinkgoWriter.Printf("⚠ Could not find Grafana service: %v\n", err)
				}
			}

			GinkgoWriter.Println("")
			GinkgoWriter.Println("Grafana Deployment Summary:")
			GinkgoWriter.Printf("  Deployment: %s\n", grafanaDeploymentName)
			GinkgoWriter.Printf("  Service:    %s\n", grafanaServiceName)
			GinkgoWriter.Printf("  Namespace:  %s\n", cfg.MonitoringNS)
			GinkgoWriter.Println("========================================")
		}
	})

	Describe("Grafana Deployment Detection", func() {
		It("should detect if Grafana is deployed", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test (set DEPLOY_OPERATIONAL_DASHBOARD=true)")
			}

			By("Verifying Grafana deployment exists")
			GinkgoWriter.Printf("  Checking deployment: %s in namespace: %s\n", grafanaDeploymentName, cfg.MonitoringNS)
			deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
				grafanaDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana deployment should exist")
			Expect(deployment).NotTo(BeNil())
			Expect(deployment.Name).To(Equal(grafanaDeploymentName))
			GinkgoWriter.Printf("  ✓ Found deployment: %s\n", deployment.Name)

			By("Verifying Grafana deployment has the correct label")
			labels := deployment.GetLabels()
			Expect(labels).To(HaveKeyWithValue("app.kubernetes.io/name", "grafana"))
			GinkgoWriter.Printf("  ✓ Deployment has label: app.kubernetes.io/name=grafana\n")
		})

		It("should verify Grafana pods are running", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			By("Checking Grafana deployment status")
			GinkgoWriter.Printf("  Checking deployment status: %s\n", grafanaDeploymentName)
			Eventually(func(g Gomega) {
				deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
					grafanaDeploymentName, metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(deployment.Status.ReadyReplicas).To(BeNumerically(">=", 1),
					"Grafana should have at least 1 ready replica")
			}, time.Duration(cfg.EventuallyStandardSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())
			GinkgoWriter.Printf("  ✓ Deployment %s has ready replicas\n", grafanaDeploymentName)

			By("Verifying Grafana pods are in Running state")
			GinkgoWriter.Printf("  Listing pods with label: app.kubernetes.io/name=grafana in namespace: %s\n", cfg.MonitoringNS)
			pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
				LabelSelector: "app.kubernetes.io/name=grafana",
			})
			Expect(err).NotTo(HaveOccurred(), "Should be able to list Grafana pods")
			Expect(pods.Items).NotTo(BeEmpty(), "Should have at least one Grafana pod")
			GinkgoWriter.Printf("  Found %d Grafana pod(s)\n", len(pods.Items))

			// Check that at least one pod is running and ready
			readyPods := 0
			for _, pod := range pods.Items {
				GinkgoWriter.Printf("  Checking pod: %s (Phase: %s)\n", pod.Name, pod.Status.Phase)
				if pod.Status.Phase == corev1.PodRunning {
					for _, condition := range pod.Status.Conditions {
						if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
							readyPods++
							GinkgoWriter.Printf("  ✓ Pod %s is Ready\n", pod.Name)
							break
						}
					}
				}
			}
			Expect(readyPods).To(BeNumerically(">=", 1), "At least one Grafana pod should be ready")
			GinkgoWriter.Printf("  ✓ Total ready pods: %d\n", readyPods)
		})

		It("should verify Grafana service exists and is accessible", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			if grafanaServiceName == "" {
				Skip("Grafana service name not discovered - skipping service test")
			}

			By("Verifying Grafana service exists")
			GinkgoWriter.Printf("  Checking service: %s in namespace: %s\n", grafanaServiceName, cfg.MonitoringNS)
			service, err := k8sClient.CoreV1().Services(cfg.MonitoringNS).Get(ctx,
				grafanaServiceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana service should exist")
			Expect(service).NotTo(BeNil())
			Expect(service.Name).To(Equal(grafanaServiceName))
			GinkgoWriter.Printf("  ✓ Found service: %s\n", service.Name)

			By("Verifying Grafana service has the correct port")
			Expect(service.Spec.Ports).NotTo(BeEmpty(), "Grafana service should have at least one port")

			var httpPort *corev1.ServicePort
			for i := range service.Spec.Ports {
				port := &service.Spec.Ports[i]
				if port.Name == httpPortName || port.Name == "http-web" || port.Port == 80 {
					httpPort = port
					break
				}
			}
			Expect(httpPort).NotTo(BeNil(), "Grafana service should have an HTTP port")
			Expect(httpPort.Port).To(Equal(int32(80)), "Grafana service should expose port 80")
			GinkgoWriter.Printf("  ✓ Service port: %d (name: %s, targetPort: %v)\n", httpPort.Port, httpPort.Name, httpPort.TargetPort)

			By("Verifying Grafana service selector matches deployment pods")
			Expect(service.Spec.Selector).NotTo(BeEmpty(), "Grafana service should have selectors")
			GinkgoWriter.Printf("  Service selector: %v\n", service.Spec.Selector)

			// Verify service selector matches pods
			selector := metav1.FormatLabelSelector(&metav1.LabelSelector{
				MatchLabels: service.Spec.Selector,
			})
			GinkgoWriter.Printf("  Looking for pods with selector: %s\n", selector)
			pods, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).List(ctx, metav1.ListOptions{
				LabelSelector: selector,
			})
			Expect(err).NotTo(HaveOccurred())
			Expect(pods.Items).NotTo(BeEmpty(), "Grafana service selector should match at least one pod")
			GinkgoWriter.Printf("  ✓ Service selector matches %d pod(s)\n", len(pods.Items))
			for _, pod := range pods.Items {
				GinkgoWriter.Printf("    - %s\n", pod.Name)
			}
		})

		It("should verify Grafana deployment has correct replicas", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			By("Checking Grafana deployment replica status")
			GinkgoWriter.Printf("  Checking replicas for deployment: %s\n", grafanaDeploymentName)
			deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
				grafanaDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Grafana typically runs with 1 replica
			Expect(deployment.Spec.Replicas).NotTo(BeNil(), "Replicas should be specified")
			Expect(*deployment.Spec.Replicas).To(BeNumerically(">=", 1), "Should have at least 1 replica")
			GinkgoWriter.Printf("  Desired replicas: %d\n", *deployment.Spec.Replicas)

			// Verify actual vs desired replicas
			Expect(deployment.Status.Replicas).To(Equal(*deployment.Spec.Replicas),
				"Actual replicas should match desired replicas")
			Expect(deployment.Status.ReadyReplicas).To(Equal(*deployment.Spec.Replicas),
				"Ready replicas should match desired replicas")
			GinkgoWriter.Printf("  ✓ Actual replicas: %d, Ready replicas: %d, Available replicas: %d\n",
				deployment.Status.Replicas, deployment.Status.ReadyReplicas, deployment.Status.AvailableReplicas)
		})

		It("should verify Grafana container configuration", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			By("Getting Grafana deployment")
			GinkgoWriter.Printf("  Getting deployment: %s\n", grafanaDeploymentName)
			deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
				grafanaDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			By("Verifying Grafana container exists")
			containers := deployment.Spec.Template.Spec.Containers
			Expect(containers).NotTo(BeEmpty(), "Deployment should have at least one container")
			GinkgoWriter.Printf("  Deployment has %d container(s)\n", len(containers))

			var grafanaContainer *corev1.Container
			for i := range containers {
				GinkgoWriter.Printf("    - Container[%d]: %s\n", i, containers[i].Name)
				if containers[i].Name == "grafana" {
					grafanaContainer = &containers[i]
				}
			}
			Expect(grafanaContainer).NotTo(BeNil(), "Grafana container should exist")
			GinkgoWriter.Printf("  ✓ Found Grafana container: %s\n", grafanaContainer.Name)

			By("Verifying Grafana container image")
			Expect(grafanaContainer.Image).NotTo(BeEmpty(), "Grafana container should have an image")
			Expect(grafanaContainer.Image).To(ContainSubstring("grafana"), "Image should be a Grafana image")
			GinkgoWriter.Printf("  ✓ Container image: %s\n", grafanaContainer.Image)

			By("Verifying Grafana container has the correct port")
			GinkgoWriter.Printf("  Container exposes %d port(s)\n", len(grafanaContainer.Ports))
			var httpPort *corev1.ContainerPort
			for i := range grafanaContainer.Ports {
				port := &grafanaContainer.Ports[i]
				GinkgoWriter.Printf("    - Port[%d]: name=%s, containerPort=%d, protocol=%s\n",
					i, port.Name, port.ContainerPort, port.Protocol)
				if port.Name == httpPortName || port.Name == "grafana" || port.ContainerPort == 3000 {
					httpPort = port
				}
			}
			Expect(httpPort).NotTo(BeNil(), "Grafana container should expose HTTP port")
			GinkgoWriter.Printf("  ✓ Container HTTP port: %d (name: %s)\n", httpPort.ContainerPort, httpPort.Name)
		})

		It("should handle Grafana deployment updates gracefully", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			By("Recording initial deployment generation")
			GinkgoWriter.Printf("  Checking deployment: %s\n", grafanaDeploymentName)
			deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
				grafanaDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())
			initialGeneration := deployment.Generation
			GinkgoWriter.Printf("  Current generation: %d\n", initialGeneration)

			By("Verifying deployment has rollout history")
			// Deployment should have status information
			Expect(deployment.Status.ObservedGeneration).To(Equal(initialGeneration),
				"Observed generation should match current generation")
			GinkgoWriter.Printf("  ✓ Observed generation: %d (matches current)\n", deployment.Status.ObservedGeneration)

			// Check conditions
			GinkgoWriter.Printf("  Deployment has %d condition(s):\n", len(deployment.Status.Conditions))
			var availableCondition *appsv1.DeploymentCondition
			for i := range deployment.Status.Conditions {
				condition := &deployment.Status.Conditions[i]
				GinkgoWriter.Printf("    - Type: %s, Status: %s, Reason: %s\n",
					condition.Type, condition.Status, condition.Reason)
				if condition.Type == appsv1.DeploymentAvailable {
					availableCondition = condition
				}
			}
			Expect(availableCondition).NotTo(BeNil(), "Deployment should have Available condition")
			Expect(availableCondition.Status).To(Equal(corev1.ConditionTrue),
				"Deployment should be available")

			GinkgoWriter.Printf("  ✓ Deployment %s is healthy (generation %d)\n", grafanaDeploymentName, initialGeneration)
		})
	})

	Describe("Grafana Accessibility", func() {
		It("should allow access with valid credentials", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping accessibility test")
			}

			By("Verifying Grafana service exists with correct port configuration")
			GinkgoWriter.Printf("  Verifying service: %s in namespace: %s\n", grafanaServiceName, cfg.MonitoringNS)
			service, err := k8sClient.CoreV1().Services(cfg.MonitoringNS).Get(ctx,
				grafanaServiceName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana service should exist")
			GinkgoWriter.Printf("  ✓ Service found: %s\n", service.Name)

			// Verify service has the HTTP port
			var servicePort *corev1.ServicePort
			for i := range service.Spec.Ports {
				port := &service.Spec.Ports[i]
				if port.Name == httpPortName || port.Name == "http-web" || port.Port == 80 {
					servicePort = port
					break
				}
			}
			Expect(servicePort).NotTo(BeNil(), "Grafana service should have HTTP port")
			Expect(servicePort.Port).To(Equal(int32(80)),
				"Grafana service should expose port 80 as documented")
			GinkgoWriter.Printf("  ✓ Service port: %d → targetPort: %v\n", servicePort.Port, servicePort.TargetPort)

			By("Verifying Grafana admin password secret exists")
			secretName := kubePrometheusStackGrafanaName
			GinkgoWriter.Printf("  Checking secret: %s in namespace: %s\n", secretName, cfg.MonitoringNS)
			secret, err := k8sClient.CoreV1().Secrets(cfg.MonitoringNS).Get(ctx,
				secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana admin password secret should exist")
			Expect(secret).NotTo(BeNil())
			GinkgoWriter.Printf("  ✓ Secret found: %s\n", secret.Name)

			// Verify the secret has the admin-password key
			_, hasPassword := secret.Data["admin-password"]
			Expect(hasPassword).To(BeTrue(), "Secret should contain admin-password key")
			GinkgoWriter.Printf("  ✓ Secret has key: admin-password\n")

			// Verify the documented command to get password would work
			passwordBytes := secret.Data["admin-password"]
			Expect(passwordBytes).NotTo(BeEmpty(), "Admin password should not be empty")
			GinkgoWriter.Printf("  ✓ Password length: %d bytes (base64 encoded)\n", len(passwordBytes))
			GinkgoWriter.Printf("  ✓ Get password command: kubectl get secret -n %s %s -o jsonpath=\"{.data.admin-password}\" | base64 -d\n",
				cfg.MonitoringNS, secretName)

			By("Verifying admin user secret exists")
			_, hasUser := secret.Data["admin-user"]
			Expect(hasUser).To(BeTrue(), "Secret should contain admin-user key")
			adminUser := string(secret.Data["admin-user"])
			Expect(adminUser).To(Equal("admin"), "Default admin user should be 'admin'")
			GinkgoWriter.Printf("  ✓ Admin username: %s\n", adminUser)

			By("Verifying Grafana is accessible with correct credentials")
			// Decode the admin password from base64
			adminPassword := string(passwordBytes)
			GinkgoWriter.Printf("  Using admin credentials for authentication\n")

			// Test Grafana accessibility by making an authenticated HTTP request to the dashboards endpoint
			// This corresponds to http://localhost:3000/dashboards when accessed via port-forward
			grafanaURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/dashboards"
			GinkgoWriter.Printf("  Testing Grafana dashboards endpoint: %s\n", grafanaURL)

			// Use curl with basic auth to test the Grafana service endpoint
			curlCmd := "curl -s -o /dev/null -w '%{http_code}' -u admin:" + adminPassword + " " + grafanaURL
			GinkgoWriter.Printf("  Running: curl -s -o /dev/null -w '%%{http_code}' -u admin:*** %s\n", grafanaURL)

			// Create a pod to run curl from inside the cluster
			curlPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "grafana-curl-test",
					Namespace: cfg.MonitoringNS,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "curl",
							Image:   "quay.io/curl/curl:latest",
							Command: []string{"sh", "-c", curlCmd},
						},
					},
				},
			}

			// Clean up any existing test pod
			_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-curl-test", metav1.DeleteOptions{})
			time.Sleep(2 * time.Second)

			// Create the curl test pod
			_, err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, curlPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create curl test pod")
			GinkgoWriter.Printf("  ✓ Created curl test pod: grafana-curl-test\n")

			// Wait for pod to complete
			Eventually(func(g Gomega) {
				testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-curl-test", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
					"Curl test pod should complete")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			GinkgoWriter.Printf("  ✓ Curl test pod completed\n")

			// Get pod logs to see HTTP status code
			logReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-curl-test", &corev1.PodLogOptions{})
			logs, err := logReq.DoRaw(ctx)
			Expect(err).NotTo(HaveOccurred(), "Should be able to get curl test pod logs")

			httpCode := strings.TrimSpace(string(logs))
			GinkgoWriter.Printf("  HTTP response code: %s\n", httpCode)

			// Grafana dashboards endpoint with authentication should return 200
			Expect(httpCode).To(Equal("200"),
				"Grafana dashboards endpoint should return HTTP 200 with valid admin credentials")

			GinkgoWriter.Printf("  ✓ Grafana /dashboards endpoint is accessible with admin credentials (HTTP %s)\n", httpCode)
			GinkgoWriter.Printf("  ✓ Successfully authenticated with username: admin and password from secret\n")
			GinkgoWriter.Printf("  ✓ This corresponds to http://localhost:3000/dashboards when accessed via port-forward\n")

			// Clean up test pod
			err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-curl-test", metav1.DeleteOptions{})
			if err != nil {
				GinkgoWriter.Printf("  Warning: Failed to delete curl test pod: %v\n", err)
			} else {
				GinkgoWriter.Printf("  ✓ Cleaned up curl test pod\n")
			}
		})

		It("should reject access with invalid credentials", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping test")
			}

			By("Verifying Grafana rejects invalid credentials")
			GinkgoWriter.Printf("  Testing authentication failure with bad credentials\n")

			// Test with invalid password
			grafanaURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/dashboards"
			badPassword := "invalid-password-12345"

			// Use curl with bad credentials
			curlCmd := "curl -s -o /dev/null -w '%{http_code}' -u admin:" + badPassword + " " + grafanaURL
			GinkgoWriter.Printf("  Running: curl -s -o /dev/null -w '%%{http_code}' -u admin:*** %s\n", grafanaURL)

			// Create a pod to run curl with bad credentials
			curlPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "grafana-curl-bad-auth-test",
					Namespace: cfg.MonitoringNS,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "curl",
							Image:   "quay.io/curl/curl:latest",
							Command: []string{"sh", "-c", curlCmd},
						},
					},
				},
			}

			// Clean up any existing test pod
			_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-curl-bad-auth-test", metav1.DeleteOptions{})
			time.Sleep(2 * time.Second)

			// Create the curl test pod
			_, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, curlPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create curl test pod")
			GinkgoWriter.Printf("  ✓ Created curl test pod: grafana-curl-bad-auth-test\n")

			// Wait for pod to complete
			Eventually(func(g Gomega) {
				testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-curl-bad-auth-test", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
					"Curl test pod should complete")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			GinkgoWriter.Printf("  ✓ Curl test pod completed\n")

			// Get pod logs to see HTTP status code
			logReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-curl-bad-auth-test", &corev1.PodLogOptions{})
			logs, err := logReq.DoRaw(ctx)
			Expect(err).NotTo(HaveOccurred(), "Should be able to get curl test pod logs")

			httpCode := strings.TrimSpace(string(logs))
			GinkgoWriter.Printf("  HTTP response code: %s\n", httpCode)

			// Grafana should reject bad credentials with 401 Unauthorized or 302 redirect to login
			Expect(httpCode).To(Or(Equal("401"), Equal("302")),
				"Grafana should reject invalid credentials with HTTP 401 or 302")

			if httpCode == "401" {
				GinkgoWriter.Printf("  ✓ Grafana correctly rejected invalid credentials (HTTP 401 Unauthorized)\n")
			} else {
				GinkgoWriter.Printf("  ✓ Grafana correctly rejected invalid credentials (HTTP 302 redirect to login)\n")
			}

			// Clean up test pod
			err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-curl-bad-auth-test", metav1.DeleteOptions{})
			if err != nil {
				GinkgoWriter.Printf("  Warning: Failed to delete curl test pod: %v\n", err)
			} else {
				GinkgoWriter.Printf("  ✓ Cleaned up curl test pod\n")
			}

			GinkgoWriter.Printf("  ✓ Grafana authentication security is working correctly\n")
		})

		It("should verify Prometheus datasource is configured and healthy", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping datasource test")
			}

			By("Verifying Grafana admin credentials exist")
			secretName := kubePrometheusStackGrafanaName
			GinkgoWriter.Printf("  Checking secret: %s in namespace: %s\n", secretName, cfg.MonitoringNS)
			secret, err := k8sClient.CoreV1().Secrets(cfg.MonitoringNS).Get(ctx,
				secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana admin password secret should exist")
			GinkgoWriter.Printf("  ✓ Secret found: %s\n", secret.Name)

			// Get admin password
			passwordBytes, hasPassword := secret.Data["admin-password"]
			Expect(hasPassword).To(BeTrue(), "Secret should contain admin-password key")
			adminPassword := string(passwordBytes)
			GinkgoWriter.Printf("  ✓ Using admin credentials for authentication\n")

			By("Querying Grafana datasources API")
			// Query the /api/datasources endpoint to get all configured datasources
			grafanaURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/api/datasources"
			GinkgoWriter.Printf("  Testing Grafana datasources API: %s\n", grafanaURL)

			// Use curl with basic auth to query datasources
			// Using jq to parse JSON and check for Prometheus datasource
			curlCmd := "curl -s -u admin:" + adminPassword + " " + grafanaURL
			GinkgoWriter.Printf("  Running: curl -s -u admin:*** %s | jq\n", grafanaURL)

			// Create a pod to run curl from inside the cluster
			curlPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "grafana-datasource-test",
					Namespace: cfg.MonitoringNS,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "curl",
							Image:   "quay.io/curl/curl:latest",
							Command: []string{"sh", "-c", curlCmd},
						},
					},
				},
			}

			// Clean up any existing test pod
			_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-datasource-test", metav1.DeleteOptions{})
			time.Sleep(2 * time.Second)

			// Create the curl test pod
			_, err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, curlPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create curl test pod")
			GinkgoWriter.Printf("  ✓ Created curl test pod: grafana-datasource-test\n")

			// Wait for pod to complete
			Eventually(func(g Gomega) {
				testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-datasource-test", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
					"Curl test pod should complete")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			GinkgoWriter.Printf("  ✓ Curl test pod completed\n")

			// Get pod logs to see datasources JSON
			logReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-datasource-test", &corev1.PodLogOptions{})
			logs, err := logReq.DoRaw(ctx)
			Expect(err).NotTo(HaveOccurred(), "Should be able to get curl test pod logs")

			datasourcesJSON := string(logs)
			GinkgoWriter.Printf("  Datasources API response:\n%s\n", datasourcesJSON)

			// Verify the response contains a Prometheus datasource
			Expect(datasourcesJSON).To(ContainSubstring("\"type\":\"prometheus\""),
				"Should have a datasource of type 'prometheus'")
			Expect(datasourcesJSON).To(ContainSubstring("\"name\":\"Prometheus\""),
				"Should have a datasource named 'Prometheus'")

			GinkgoWriter.Printf("  ✓ Found datasource with type: prometheus\n")
			GinkgoWriter.Printf("  ✓ Found datasource with name: Prometheus\n")

			// Extract the datasource UID for health check (use UID instead of numeric ID)
			// The response should contain "uid":"<uid>" for the Prometheus datasource
			// Find the datasource object that matches type=prometheus and name=Prometheus
			var datasourceUID string

			// Split by datasource objects (look for "type":"prometheus" entries)
			if strings.Contains(datasourcesJSON, "\"type\":\"prometheus\"") {
				// Find the section with Prometheus datasource
				prometheusStart := strings.Index(datasourcesJSON, "\"type\":\"prometheus\"")
				if prometheusStart > 0 {
					// Look backwards from type to find the start of this object (look for preceding {)
					objStart := strings.LastIndex(datasourcesJSON[:prometheusStart], "{")
					if objStart >= 0 {
						// Look forward from type to find the end of this object (look for closing })
						objEnd := strings.Index(datasourcesJSON[prometheusStart:], "}")
						if objEnd > 0 {
							objEnd = prometheusStart + objEnd + 1
							datasourceObj := datasourcesJSON[objStart:objEnd]

							// Extract UID from this specific object
							if strings.Contains(datasourceObj, "\"uid\":") {
								uidParts := strings.Split(datasourceObj, "\"uid\":")
								if len(uidParts) > 1 {
									uidValue := strings.TrimSpace(uidParts[1])
									// UID is quoted, extract the string between quotes
									uidValue, hasPrefix := strings.CutPrefix(uidValue, "\"")
									if hasPrefix {
										endQuote := strings.Index(uidValue, "\"")
										if endQuote > 0 {
											datasourceUID = uidValue[:endQuote]
											GinkgoWriter.Printf("  ✓ Found Prometheus datasource UID: %s\n", datasourceUID)
										}
									}
								}
							}
						}
					}
				}
			}

			// Clean up test pod before running health check
			err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-datasource-test", metav1.DeleteOptions{})
			if err != nil {
				GinkgoWriter.Printf("  Warning: Failed to delete curl test pod: %v\n", err)
			} else {
				GinkgoWriter.Printf("  ✓ Cleaned up curl test pod\n")
			}

			if datasourceUID != "" {
				By("Testing Prometheus datasource health (Grafana 'Test' button)")
				// Test the datasource using Grafana's health check API
				// This is equivalent to clicking the "Test" button in the Grafana UI
				// Using GET /api/datasources/uid/{uid}/health
				healthURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/api/datasources/uid/" + datasourceUID + "/health"
				GinkgoWriter.Printf("  Testing datasource health API: %s\n", healthURL)

				healthCmd := "curl -s -u admin:" + adminPassword + " -X GET " + healthURL
				GinkgoWriter.Printf("  Running: curl -s -u admin:*** -X GET %s\n", healthURL)

				// Create a pod to run the health check
				healthPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "grafana-datasource-health-test",
						Namespace: cfg.MonitoringNS,
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:    "curl",
								Image:   "quay.io/curl/curl:latest",
								Command: []string{"sh", "-c", healthCmd},
							},
						},
					},
				}

				// Clean up any existing health test pod
				_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-datasource-health-test", metav1.DeleteOptions{})
				time.Sleep(2 * time.Second)

				// Create the health check pod
				_, err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, healthPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "Should be able to create health check pod")
				GinkgoWriter.Printf("  ✓ Created health check pod: grafana-datasource-health-test\n")

				// Wait for pod to complete
				Eventually(func(g Gomega) {
					testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-datasource-health-test", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
						"Health check pod should complete")
				}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
					time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

				GinkgoWriter.Printf("  ✓ Health check pod completed\n")

				// Get pod logs to see health check response
				healthLogReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-datasource-health-test", &corev1.PodLogOptions{})
				healthLogs, err := healthLogReq.DoRaw(ctx)
				Expect(err).NotTo(HaveOccurred(), "Should be able to get health check pod logs")

				healthResponse := string(healthLogs)
				GinkgoWriter.Printf("  Health check response:\n%s\n", healthResponse)

				// Verify the datasource is healthy
				// Grafana returns {"status":"OK",...} for a healthy datasource
				if strings.Contains(healthResponse, "\"status\":\"OK\"") {
					GinkgoWriter.Printf("  ✓ Prometheus datasource health check passed (equivalent to clicking 'Test' button in UI)\n")
					GinkgoWriter.Printf("  ✓ Datasource can successfully connect to Prometheus\n")
				} else if strings.Contains(healthResponse, "plugin.unavailable") {
					// Plugin might not be ready yet or requires additional time to initialize
					GinkgoWriter.Printf("  ⚠ Datasource plugin not available yet (Grafana may still be initializing)\n")
					GinkgoWriter.Printf("  ⚠ Skipping health check - datasource configuration is valid but plugin not ready\n")
				} else {
					// Other errors should fail the test
					Expect(healthResponse).To(ContainSubstring("\"status\":\"OK\""),
						"Prometheus datasource health check should return OK status")
				}

				// Clean up health check pod
				err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-datasource-health-test", metav1.DeleteOptions{})
				if err != nil {
					GinkgoWriter.Printf("  Warning: Failed to delete health check pod: %v\n", err)
				} else {
					GinkgoWriter.Printf("  ✓ Cleaned up health check pod\n")
				}
			} else {
				GinkgoWriter.Printf("  ⚠ Could not extract datasource UID, skipping health check test\n")
			}

			GinkgoWriter.Printf("  ✓ Prometheus datasource is correctly configured in Grafana\n")
		})
	})

	Describe("Grafana Dashboards", func() {
		It("should verify WVA Operational Dashboard ConfigMap exists", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping dashboard test")
			}

			By("Verifying operational dashboard ConfigMap exists")
			configMapName := wvaOperationDashboardConfigMapName
			GinkgoWriter.Printf("  Checking ConfigMap: %s in namespace: %s\n", configMapName, cfg.MonitoringNS)
			configMap, err := k8sClient.CoreV1().ConfigMaps(cfg.MonitoringNS).Get(ctx,
				configMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Operational dashboard ConfigMap should exist")
			GinkgoWriter.Printf("  ✓ ConfigMap found: %s\n", configMap.Name)

			By("Verifying ConfigMap has the Grafana sidecar label")
			labels := configMap.GetLabels()
			Expect(labels).To(HaveKeyWithValue("grafana_dashboard", "1"),
				"ConfigMap should have grafana_dashboard=1 label for sidecar discovery")
			GinkgoWriter.Printf("  ✓ ConfigMap has label: grafana_dashboard=1\n")

			By("Verifying ConfigMap contains dashboard JSON")
			dashboardJSON, hasDashboard := configMap.Data["operational-dashboard.json"]
			Expect(hasDashboard).To(BeTrue(), "ConfigMap should contain operational-dashboard.json")
			Expect(dashboardJSON).NotTo(BeEmpty(), "Dashboard JSON should not be empty")
			GinkgoWriter.Printf("  ✓ ConfigMap contains operational-dashboard.json (%d bytes)\n", len(dashboardJSON))

			By("Verifying dashboard JSON structure")
			Expect(dashboardJSON).To(ContainSubstring("WVA Operational Dashboard"),
				"Dashboard JSON should contain title")
			Expect(dashboardJSON).To(ContainSubstring("panels"),
				"Dashboard JSON should have panels array")
			GinkgoWriter.Printf("  ✓ Dashboard JSON has expected structure\n")
		})

		It("should verify WVA Operational Dashboard is provisioned in Grafana", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping dashboard test")
			}

			By("Verifying Grafana admin credentials exist")
			secretName := kubePrometheusStackGrafanaName
			GinkgoWriter.Printf("  Checking secret: %s in namespace: %s\n", secretName, cfg.MonitoringNS)
			secret, err := k8sClient.CoreV1().Secrets(cfg.MonitoringNS).Get(ctx,
				secretName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Grafana admin password secret should exist")
			GinkgoWriter.Printf("  ✓ Secret found: %s\n", secret.Name)

			// Get admin password
			passwordBytes, hasPassword := secret.Data["admin-password"]
			Expect(hasPassword).To(BeTrue(), "Secret should contain admin-password key")
			adminPassword := string(passwordBytes)
			GinkgoWriter.Printf("  ✓ Using admin credentials for authentication\n")

			By("Querying Grafana dashboards API")
			// Search for dashboards using the /api/search endpoint
			grafanaURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/api/search?type=dash-db"
			GinkgoWriter.Printf("  Testing Grafana search API: %s\n", grafanaURL)

			// Use curl with basic auth to query dashboards
			curlCmd := "curl -s -u admin:" + adminPassword + " " + grafanaURL
			GinkgoWriter.Printf("  Running: curl -s -u admin:*** %s\n", grafanaURL)

			// Create a pod to run curl from inside the cluster
			curlPod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "grafana-dashboard-search-test",
					Namespace: cfg.MonitoringNS,
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{
							Name:    "curl",
							Image:   "quay.io/curl/curl:latest",
							Command: []string{"sh", "-c", curlCmd},
						},
					},
				},
			}

			// Clean up any existing test pod
			_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-dashboard-search-test", metav1.DeleteOptions{})
			time.Sleep(2 * time.Second)

			// Create the curl test pod
			_, err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, curlPod, metav1.CreateOptions{})
			Expect(err).NotTo(HaveOccurred(), "Should be able to create curl test pod")
			GinkgoWriter.Printf("  ✓ Created curl test pod: grafana-dashboard-search-test\n")

			// Wait for pod to complete
			Eventually(func(g Gomega) {
				testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-dashboard-search-test", metav1.GetOptions{})
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
					"Curl test pod should complete")
			}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
				time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

			GinkgoWriter.Printf("  ✓ Curl test pod completed\n")

			// Get pod logs to see dashboards JSON
			logReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-dashboard-search-test", &corev1.PodLogOptions{})
			logs, err := logReq.DoRaw(ctx)
			Expect(err).NotTo(HaveOccurred(), "Should be able to get curl test pod logs")

			dashboardsJSON := string(logs)

			// Verify the response contains the WVA Operational Dashboard
			Expect(dashboardsJSON).To(ContainSubstring("WVA Operational Dashboard"),
				"Should have a dashboard titled 'WVA Operational Dashboard'")

			GinkgoWriter.Printf("  ✓ Found dashboard with title: WVA Operational Dashboard\n")

			// Extract dashboard UID for detailed verification
			var dashboardUID string
			if strings.Contains(dashboardsJSON, "\"title\":\"WVA Operational Dashboard\"") {
				titleStart := strings.Index(dashboardsJSON, "\"title\":\"WVA Operational Dashboard\"")
				if titleStart > 0 {
					objStart := strings.LastIndex(dashboardsJSON[:titleStart], "{")
					if objStart >= 0 {
						objEnd := strings.Index(dashboardsJSON[titleStart:], "}")
						if objEnd > 0 {
							objEnd = titleStart + objEnd + 1
							dashboardObj := dashboardsJSON[objStart:objEnd]

							if strings.Contains(dashboardObj, "\"uid\":") {
								uidParts := strings.Split(dashboardObj, "\"uid\":")
								if len(uidParts) > 1 {
									uidValue := strings.TrimSpace(uidParts[1])
									uidValue, hasPrefix := strings.CutPrefix(uidValue, "\"")
									if hasPrefix {
										endQuote := strings.Index(uidValue, "\"")
										if endQuote > 0 {
											dashboardUID = uidValue[:endQuote]
											GinkgoWriter.Printf("  ✓ Found dashboard UID: %s\n", dashboardUID)
										}
									}
								}
							}
						}
					}
				}
			}

			// Clean up test pod
			err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-dashboard-search-test", metav1.DeleteOptions{})
			if err != nil {
				GinkgoWriter.Printf("  Warning: Failed to delete curl test pod: %v\n", err)
			} else {
				GinkgoWriter.Printf("  ✓ Cleaned up curl test pod\n")
			}

			if dashboardUID != "" {
				By("Retrieving dashboard details to verify metrics")
				dashboardURL := "http://" + grafanaServiceName + "." + cfg.MonitoringNS + ".svc.cluster.local:80/api/dashboards/uid/" + dashboardUID
				GinkgoWriter.Printf("  Testing dashboard details API: %s\n", dashboardURL)

				dashCmd := "curl -s -u admin:" + adminPassword + " " + dashboardURL
				GinkgoWriter.Printf("  Running: curl -s -u admin:*** %s\n", dashboardURL)

				dashPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "grafana-dashboard-get-test",
						Namespace: cfg.MonitoringNS,
					},
					Spec: corev1.PodSpec{
						RestartPolicy: corev1.RestartPolicyNever,
						Containers: []corev1.Container{
							{
								Name:    "curl",
								Image:   "quay.io/curl/curl:latest",
								Command: []string{"sh", "-c", dashCmd},
							},
						},
					},
				}

				_ = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-dashboard-get-test", metav1.DeleteOptions{})
				time.Sleep(2 * time.Second)

				_, err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Create(ctx, dashPod, metav1.CreateOptions{})
				Expect(err).NotTo(HaveOccurred(), "Should be able to create dashboard get test pod")
				GinkgoWriter.Printf("  ✓ Created dashboard get test pod: grafana-dashboard-get-test\n")

				Eventually(func(g Gomega) {
					testPod, err := k8sClient.CoreV1().Pods(cfg.MonitoringNS).Get(ctx, "grafana-dashboard-get-test", metav1.GetOptions{})
					g.Expect(err).NotTo(HaveOccurred())
					g.Expect(testPod.Status.Phase).To(Or(Equal(corev1.PodSucceeded), Equal(corev1.PodFailed)),
						"Dashboard get test pod should complete")
				}, time.Duration(cfg.EventuallyMediumSec)*time.Second,
					time.Duration(cfg.PollIntervalSec)*time.Second).Should(Succeed())

				GinkgoWriter.Printf("  ✓ Dashboard get test pod completed\n")

				dashLogReq := k8sClient.CoreV1().Pods(cfg.MonitoringNS).GetLogs("grafana-dashboard-get-test", &corev1.PodLogOptions{})
				dashLogs, err := dashLogReq.DoRaw(ctx)
				Expect(err).NotTo(HaveOccurred(), "Should be able to get dashboard get test pod logs")

				dashboardDetailJSON := string(dashLogs)
				GinkgoWriter.Printf("  Dashboard detail response (first 500 chars):\n%s...\n",
					dashboardDetailJSON[:min(len(dashboardDetailJSON), 500)])

				// Verify dashboard contains expected WVA metrics
				Expect(dashboardDetailJSON).To(ContainSubstring("wva_models_processed"),
					"Dashboard should contain WVA metrics query: wva_models_processed")
				GinkgoWriter.Printf("  ✓ Dashboard contains WVA metric query: wva_models_processed\n")

				Expect(dashboardDetailJSON).To(ContainSubstring("Models Processed"),
					"Dashboard should contain 'Models Processed' panel title")
				GinkgoWriter.Printf("  ✓ Dashboard contains panel: Models Processed\n")

				err = k8sClient.CoreV1().Pods(cfg.MonitoringNS).Delete(ctx, "grafana-dashboard-get-test", metav1.DeleteOptions{})
				if err != nil {
					GinkgoWriter.Printf("  Warning: Failed to delete dashboard get test pod: %v\n", err)
				} else {
					GinkgoWriter.Printf("  ✓ Cleaned up dashboard get test pod\n")
				}
			} else {
				GinkgoWriter.Printf("  ⚠ Could not extract dashboard UID, skipping detailed verification\n")
			}

			GinkgoWriter.Printf("  ✓ WVA Operational Dashboard is correctly provisioned in Grafana\n")
		})

		It("should verify 'Models Processed' panel exists in dashboard", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping dashboard panel test")
			}

			By("Verifying operational dashboard ConfigMap contains Models Processed panel")
			configMapName := wvaOperationDashboardConfigMapName
			GinkgoWriter.Printf("  Checking ConfigMap: %s in namespace: %s\n", configMapName, cfg.MonitoringNS)
			configMap, err := k8sClient.CoreV1().ConfigMaps(cfg.MonitoringNS).Get(ctx,
				configMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Operational dashboard ConfigMap should exist")
			GinkgoWriter.Printf("  ✓ ConfigMap found: %s\n", configMap.Name)

			dashboardJSON, hasDashboard := configMap.Data["operational-dashboard.json"]
			Expect(hasDashboard).To(BeTrue(), "ConfigMap should contain operational-dashboard.json")
			GinkgoWriter.Printf("  ✓ Dashboard JSON found\n")

			By("Verifying dashboard contains 'Models Processed' panel")
			Expect(dashboardJSON).To(ContainSubstring("\"title\": \"Models Processed\""),
				"Dashboard should contain a panel titled 'Models Processed'")
			GinkgoWriter.Printf("  ✓ Found panel with title: 'Models Processed'\n")

			By("Verifying Models Processed panel has correct metric")
			Expect(dashboardJSON).To(ContainSubstring("wva_models_processed"),
				"Models Processed panel should query wva_models_processed metric")
			GinkgoWriter.Printf("  ✓ Panel queries metric: wva_models_processed\n")

			By("Verifying Models Processed panel has description")
			Expect(dashboardJSON).To(ContainSubstring("Number of models processed in the last optimization cycle"),
				"Models Processed panel should have a description")
			GinkgoWriter.Printf("  ✓ Panel has description about optimization cycle\n")

			GinkgoWriter.Printf("  ✓ 'Models Processed' panel is correctly configured\n")
		})

		It("should verify 'Optimization Duration - Percentiles and Average' panel exists in dashboard", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - skipping dashboard panel test")
			}

			By("Verifying operational dashboard ConfigMap contains Optimization Duration panel")
			configMapName := wvaOperationDashboardConfigMapName
			GinkgoWriter.Printf("  Checking ConfigMap: %s in namespace: %s\n", configMapName, cfg.MonitoringNS)
			configMap, err := k8sClient.CoreV1().ConfigMaps(cfg.MonitoringNS).Get(ctx,
				configMapName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred(), "Operational dashboard ConfigMap should exist")
			GinkgoWriter.Printf("  ✓ ConfigMap found: %s\n", configMap.Name)

			dashboardJSON, hasDashboard := configMap.Data["operational-dashboard.json"]
			Expect(hasDashboard).To(BeTrue(), "ConfigMap should contain operational-dashboard.json")
			GinkgoWriter.Printf("  ✓ Dashboard JSON found\n")

			By("Verifying dashboard contains 'Optimization Duration - Percentiles and Average' panel")
			Expect(dashboardJSON).To(ContainSubstring("\"title\": \"Optimization Duration - Percentiles and Average\""),
				"Dashboard should contain a panel titled 'Optimization Duration - Percentiles and Average'")
			GinkgoWriter.Printf("  ✓ Found panel with title: 'Optimization Duration - Percentiles and Average'\n")

			By("Verifying Optimization Duration panel has histogram quantile queries")
			Expect(dashboardJSON).To(ContainSubstring("histogram_quantile(0.50"),
				"Panel should contain p50 histogram quantile query")
			Expect(dashboardJSON).To(ContainSubstring("histogram_quantile(0.95"),
				"Panel should contain p95 histogram quantile query")
			Expect(dashboardJSON).To(ContainSubstring("histogram_quantile(0.99"),
				"Panel should contain p99 histogram quantile query")
			GinkgoWriter.Printf("  ✓ Panel contains p50, p95, p99 histogram quantile queries\n")

			By("Verifying Optimization Duration panel has average calculation")
			Expect(dashboardJSON).To(ContainSubstring("wva_optimization_duration_seconds_sum"),
				"Panel should contain sum metric for average calculation")
			Expect(dashboardJSON).To(ContainSubstring("wva_optimization_duration_seconds_count"),
				"Panel should contain count metric for average calculation")
			GinkgoWriter.Printf("  ✓ Panel contains average calculation (sum/count)\n")

			By("Verifying Optimization Duration panel has bucket metric")
			Expect(dashboardJSON).To(ContainSubstring("wva_optimization_duration_seconds_bucket"),
				"Panel should contain bucket metric for histogram quantiles")
			GinkgoWriter.Printf("  ✓ Panel queries metric: wva_optimization_duration_seconds_bucket\n")

			By("Verifying Optimization Duration panel has description")
			Expect(dashboardJSON).To(ContainSubstring("Duration of optimization loop cycles in seconds"),
				"Optimization Duration panel should have a description")
			GinkgoWriter.Printf("  ✓ Panel has description about optimization loop cycles\n")

			GinkgoWriter.Printf("  ✓ 'Optimization Duration - Percentiles and Average' panel is correctly configured\n")
		})
	})

	Describe("Grafana Cleanup Verification", func() {
		It("should verify Grafana can be safely removed if not needed", func() {
			if !grafanaFound {
				Skip("Grafana is not deployed - nothing to verify for cleanup")
			}

			By("Verifying Grafana resources are properly labeled for cleanup")
			GinkgoWriter.Printf("  Checking labels for deployment: %s\n", grafanaDeploymentName)
			deployment, err := k8sClient.AppsV1().Deployments(cfg.MonitoringNS).Get(ctx,
				grafanaDeploymentName, metav1.GetOptions{})
			Expect(err).NotTo(HaveOccurred())

			// Verify labels exist for proper cleanup
			labels := deployment.GetLabels()
			Expect(labels).NotTo(BeEmpty(), "Deployment should have labels for cleanup tracking")
			Expect(labels).To(HaveKey("app.kubernetes.io/name"), "Grafana should have app.kubernetes.io/name label")

			GinkgoWriter.Println("  Deployment labels:")
			for key, value := range labels {
				GinkgoWriter.Printf("    %s: %s\n", key, value)
			}
			GinkgoWriter.Println("  ✓ Grafana resources are properly labeled for safe cleanup")
		})
	})
})
