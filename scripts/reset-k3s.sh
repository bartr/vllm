#!/bin/bash

script_dir="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
cd "$script_dir" || exit 1

sudo /usr/local/bin/k3s-uninstall.sh
rm -f ~/.kube/config
sudo rm -rf /etc/rancher/k3s

# optional aggressive wipe if you want to remove all cluster state, images, PVC data, and CNI leftovers
#sudo rm -rf /var/lib/rancher /var/lib/kubelet /etc/cni /var/lib/cni

curl -sfL https://get.k3s.io | sh -
./config.sh

cd ../cllm
make deploy

kubectl get nodes
