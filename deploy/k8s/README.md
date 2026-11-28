# Kubernetes Deployment Examples

These Kustomize manifests deploy `krakozhia` and a local GPU `llama-server` on Kubernetes.

## Prerequisites

- A PostgreSQL database with the `pgvector` extension enabled (`CREATE EXTENSION vector;`).
- A GPU node with a Kubernetes device plugin (this example uses AMD Vulkan `amd.com/gpu`; for NVIDIA, switch the image to a CUDA build and change `amd.com/gpu` to `nvidia.com/gpu`).
- A `ReadWriteOnce` StorageClass with at least `20Gi` available for model weights.

## Deploy Inference (`llama-server`)

Review [inference/deployment.yaml](inference/deployment.yaml) and [inference/pvc.yaml](inference/pvc.yaml) to set your GPU `nodeSelector` and `storageClassName` if needed, then apply:

```sh
kubectl apply -k deploy/k8s/inference
kubectl -n inference rollout status deploy/llama-server --timeout=600s
```

The `model-downloader` init container downloads `Qwen3-Embedding-0.6B-Q8_0.gguf`, `gemma-4-12B-it-qat-UD-Q4_K_XL.gguf`, and `bge-reranker-v2-m3-Q8_0.gguf` into the `llama-models` PVC on first start.

## Deploy `krakozhia`

Create the namespace and `krakozhia-config` Secret (or manage it with SOPS/KSOPS or External Secrets using [krakozhia/secret.example.yaml](krakozhia/secret.example.yaml) as a template):

```sh
kubectl apply -f deploy/k8s/krakozhia/namespace.yaml
kubectl -n krakozhia create secret generic krakozhia-config \
  --from-literal=KRAKOZHIA_DB_DSN='postgres://krakozhia:CHANGE_ME@postgres.database.svc.cluster.local:5432/krakozhia?sslmode=require' \
  --from-literal=KRAKOZHIA_API_TOKEN="$(openssl rand -hex 32)" \
  --from-literal=KRAKOZHIA_GITHUB_TOKEN='CHANGE_ME_OPTIONAL_READONLY_PAT'
```

Edit [krakozhia/deployment.yaml](krakozhia/deployment.yaml) to pin an immutable `sha-<commit>` image tag and set `KRAKOZHIA_GITHUB_OWNERS` or `KRAKOZHIA_GITHUB_REPOS` if you want GitHub roadmap sync, then apply:

```sh
kubectl apply -k deploy/k8s/krakozhia
kubectl -n krakozhia rollout status deploy/krakozhia --timeout=180s
```

To expose `krakozhia` through Gateway API, copy [krakozhia/httproute.example.yaml](krakozhia/httproute.example.yaml), replace the gateway name and hostname, and apply it.

## Verify

```sh
kubectl -n inference get pods
kubectl -n krakozhia get pods,svc
kubectl -n krakozhia port-forward svc/krakozhia 8080:8080 &
curl -fsS http://127.0.0.1:8080/healthz
```
