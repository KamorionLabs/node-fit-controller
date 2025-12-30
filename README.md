# Node Fit Controller

A Kubernetes controller that automatically adjusts pod resource limits in-place based on node availability, leveraging the **In-Place Pod Vertical Scaling** feature (GA in Kubernetes 1.35+).

## Problem Statement

When pods have memory limits that exceed the node's allocatable resources, the Linux OOM killer may terminate critical system processes (like kubelet) instead of the workload containers. This leads to nodes becoming `NotReady` and cascading failures.

**Node Fit Controller** solves this by automatically adjusting pod limits to fit within the actual node capacity.

## Requirements

- Kubernetes 1.35+ (In-Place Pod Vertical Scaling GA)
- Pods must opt-in via annotations

## Installation

```bash
# Clone the repository
git clone https://github.com/KamorionLabs/node-fit-controller.git
cd node-fit-controller

# Build and deploy
make docker-build docker-push IMG=<your-registry>/node-fit-controller:latest
make deploy IMG=<your-registry>/node-fit-controller:latest
```

## Usage

### Opt-in via Annotations

Add the `nodefit.io/enabled: "true"` annotation to pods you want the controller to manage:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
  annotations:
    nodefit.io/enabled: "true"
    nodefit.io/strategy: "percent"  # percent, fit, or cap
    nodefit.io/percent: "80"        # For percent strategy
spec:
  containers:
  - name: app
    image: my-app:latest
    resources:
      requests:
        memory: "2Gi"
        cpu: "1000m"
      limits:
        memory: "4Gi"  # Will be adjusted if exceeds node capacity
```

### Strategies

#### 1. Percent Strategy (Default)

Calculates limits as a percentage of node allocatable resources divided by the number of pods:

```
limit = min(original_limit, (node_allocatable * percent / 100) / pods_on_node)
```

**Annotations:**
- `nodefit.io/strategy: "percent"`
- `nodefit.io/percent: "80"` (default: 80)

**Example:**
```yaml
annotations:
  nodefit.io/enabled: "true"
  nodefit.io/strategy: "percent"
  nodefit.io/percent: "70"
```

#### 2. Fit Strategy

Calculates limits based on actual available resources after accounting for other pods:

```
limit = node_allocatable - sum(other_pods_requests) - buffer
```

**Annotations:**
- `nodefit.io/strategy: "fit"`
- `nodefit.io/buffer: "256Mi"` (default: 256Mi)

**Example:**
```yaml
annotations:
  nodefit.io/enabled: "true"
  nodefit.io/strategy: "fit"
  nodefit.io/buffer: "512Mi"
```

#### 3. Cap Strategy

Sets limits equal to requests, preventing any burst capacity:

```
limit = request
```

**Annotations:**
- `nodefit.io/strategy: "cap"`

**Example:**
```yaml
annotations:
  nodefit.io/enabled: "true"
  nodefit.io/strategy: "cap"
```

## Annotation Reference

| Annotation | Description | Default | Values |
|------------|-------------|---------|--------|
| `nodefit.io/enabled` | Enable node-fit for this pod | - | `"true"` |
| `nodefit.io/strategy` | Calculation strategy | `percent` | `percent`, `fit`, `cap` |
| `nodefit.io/percent` | Percentage for percent strategy | `80` | `1-100` |
| `nodefit.io/buffer` | Buffer for fit strategy | `256Mi` | Quantity (e.g., `512Mi`, `1Gi`) |
| `nodefit.io/adjusted` | Set by controller when limits were adjusted | - | `"true"` (read-only) |

## Example: WordPress on Small Nodes

Problem: WordPress pods with 4Gi limits scheduled on t3a.medium nodes (4GB RAM total, ~3.3GB allocatable) cause OOM kills.

Solution:
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: wordpress
spec:
  template:
    metadata:
      annotations:
        nodefit.io/enabled: "true"
        nodefit.io/strategy: "fit"
        nodefit.io/buffer: "512Mi"
    spec:
      containers:
      - name: wordpress
        resources:
          requests:
            memory: "2500Mi"
          limits:
            memory: "4Gi"  # Will be reduced to ~2.8Gi on t3a.medium
```

## How It Works

1. Controller watches pods with `nodefit.io/enabled: "true"` annotation
2. When a pod is scheduled and running, it fetches the node's allocatable resources
3. Based on the configured strategy, it calculates appropriate limits
4. Uses Kubernetes 1.35+ In-Place Pod Vertical Scaling to patch limits without restart
5. Adds `nodefit.io/adjusted: "true"` annotation to mark processed pods

## Limitations

- Requires Kubernetes 1.35+ for in-place resource updates
- Only adjusts limits, not requests
- CPU limits are only set if already present (best practice is to not set CPU limits)
- Works only on running pods (not pending)

## Development

```bash
# Run locally against current kubeconfig context
make run

# Run tests
make test

# Generate manifests
make manifests
```

## License

Copyright 2025 KamorionLabs.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

## Contributing

Contributions welcome! Please open an issue or PR on GitHub.
