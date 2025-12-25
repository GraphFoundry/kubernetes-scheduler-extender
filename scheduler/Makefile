.PHONY: help build build-scheduler build-extender docker-build docker-build-scheduler docker-build-extender deploy deploy-base deploy-scheduler delete delete-all clean minikube-setup logs status restart redeploy setup

# Variables
SCHEDULER_IMAGE := my-scheduler
IMAGE_TAG := test
NAMESPACE := kube-system

docker-build-scheduler: ## Build Docker image for scheduler
	@echo "Building Scheduler Docker image..."
	docker build -t $(SCHEDULER_IMAGE):$(IMAGE_TAG) .
	@echo "Docker image built: $(SCHEDULER_IMAGE):$(IMAGE_TAG)"

run: ## Build Docker images, load to minikube, and restart scheduler
	make docker-build-scheduler
	@if minikube image ls | grep -q "$(SCHEDULER_IMAGE)"; then \
		kubectl delete deploy my-scheduler -n kube-system --ignore-not-found; \
	fi
	minikube image load $(SCHEDULER_IMAGE):$(IMAGE_TAG)
	kubectl apply -f ../deployments/scheduler.yaml
	kubectl rollout restart deployment/my-scheduler -n $(NAMESPACE)
