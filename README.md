# My Custom Scheduler

## Deploying to Minikube

Follow these steps to deploy the custom scheduler to your local Minikube cluster.

### 1. Prerequisites
- Minikube running (`minikube start`)
- kubectl installed and configured
- A service running on your host machine at port 3000 (referenced as `host.minikube.internal:3000` by the scheduler).

### 2. Build the Docker Image
We need to build the image _inside_ Minikube's Docker environment so Kubernetes can find it.

```bash
eval $(minikube docker-env)
docker build -t my-scheduler:test .
```

### 3. Deploy Resources
Apply the RBAC, ServiceAccount, and Deployment manifests.

```bash
kubectl apply -f deployments/scheduler.yaml
```

### 4. Verify Deployment
Check if the scheduler pod is running in the `kube-system` namespace.

```bash
kubectl get pods -n kube-system -l app=my-scheduler
```

You can view the logs to see it starting up:

```bash
kubectl logs -n kube-system -l app=my-scheduler -f
```

### 5. Run a Test Pod
Deploy a pod that requests `my-scheduler`.

```bash
kubectl apply -f test-pod.yaml
```

Watch it get scheduled:

```bash
kubectl get pods test-pod -o wide -w
```
