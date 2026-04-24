#!/bin/bash

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir" || exit 1

sudo /usr/local/bin/k3s-uninstall.sh
rm -f ~/.kube/config
sudo rm -rf /etc/rancher/k3s

# optional aggressive wipe if you want to remove all cluster state, images, PVC data, and CNI leftovers
#sudo rm -rf /var/lib/rancher /var/lib/kubelet /etc/cni /var/lib/cni

curl -sfL https://get.k3s.io | sh -

sudo cp /etc/rancher/k3s/k3s.yaml ~/.kube/config
sudo chown $(id -u):$(id -g) -R ~/.kube

cd ../cllm
make deploy

cd ../clusters/z01/flux-system
kubectl apply -f components.yaml
kubectl apply -f source.yaml
kubectl wait --namespace flux-system --for=condition=Ready pod --all --timeout=60s

kubectl apply -f listeners/nvidia-device-plugin.yaml
flux reconcile kustomization nvidia-plugin
kubectl wait --namespace nvidia-device-plugin --for=condition=Ready pod --all --timeout=300s

kubectl apply -f listeners/vllm.yaml
flux reconcile kustomization vllm
kubectl wait --namespace vllm --for=condition=Ready pod --all --timeout=15m
