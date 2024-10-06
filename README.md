# k8s-node-drainer
![download](https://github.com/user-attachments/assets/5ecf64fb-3a9f-4dbf-9dd8-aa4795a6319e)

`k8s-node-drainer` is a tool designed to drain Kubernetes high CPU utilized nodes in a safe and automated way. It ensures that your nodes are drained properly, taking care of evicting all running pods, respecting Pod Disruption Budgets (PDBs), and handling long-running processes efficiently.

## Features

- Automatically drains Kubernetes nodes.
- Respects Pod Disruption Budgets (PDBs).
- Graceful handling of long-running processes.
- Configurable via Helm charts for easy deployment.

## Prerequisites

- Kubernetes cluster
- `kubectl` access to the cluster
- Go (for local development)
- Docker (for containerization)
- Helm (for Kubernetes deployment)

## Installation

### Using Helm

To deploy the `k8s-node-drainer` using Helm, follow these steps:

1. Install the Helm chart:

    ```bash
    helm install oci://ghcr.io/matanbaruch/charts/node-drainer --generate-name
    ```

1. Verify the deployment:

    ```bash
    kubectl get pods
    ```

## Configuration

The Helm chart is highly configurable via the `values.yaml` file. Some key configuration options include:

- `replicaCount`: Number of replicas of the node drainer.
- `image.repository`: The Docker image to use for the node drainer.
- `resources`: Resource requests and limits for the node drainer pod.
- `tolerations`: Define tolerations if your nodes have taints.

## Usage

To drain a node manually, use the following command:

```bash
kubectl drain <node-name> --ignore-daemonsets --delete-emptydir-data
```

This tool automates the above process, ensuring safe and controlled node draining.

## Development

### Prerequisites

- Go 1.22+
- Docker
- Kubernetes (for testing)

### Steps

1. Clone the repository:

    ```bash
    git clone https://github.com/<your-username>/k8s-node-drainer.git
    cd k8s-node-drainer
    ```

2. Install dependencies:

    ```bash
    go mod download
    ```

3. Build the binary:

    ```bash
    go build -o k8s-node-drainer main.go
    ```

4. Run the tool locally:

    ```bash
    ./k8s-node-drainer
    ```

## License

This project is licensed under the MIT License. See the [LICENSE](LICENSE) file for more details.

---

Let me know if you would like to adjust any part of this `README.md` or if you'd like it saved in the project files.
