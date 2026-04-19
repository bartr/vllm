#!/bin/bash

set -euo pipefail

if [ "${EUID}" -ne 0 ]; then
    echo "Run with sudo ./install-nvidia-container-toolkit.sh"
    exit 1
fi

source /etc/os-release

if [[ "${ID}" != "ubuntu" && "${ID}" != "debian" ]]; then
    echo "unsupported distro: ${ID}"
    exit 1
fi

install -m 0755 -d /etc/apt/keyrings
curl -fsSL https://nvidia.github.io/libnvidia-container/gpgkey \
    | gpg --dearmor -o /etc/apt/keyrings/nvidia-container-toolkit-keyring.gpg
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/deb/nvidia-container-toolkit.list \
    | sed 's#deb https://#deb [signed-by=/etc/apt/keyrings/nvidia-container-toolkit-keyring.gpg] https://#' \
    > /etc/apt/sources.list.d/nvidia-container-toolkit.list

apt-get update
apt-get install -y nvidia-container-toolkit

if ! command -v nvidia-container-runtime >/dev/null 2>&1; then
    echo "nvidia-container-runtime was not installed correctly"
    exit 1
fi

systemctl restart k3s

echo
echo "Installed NVIDIA container toolkit and restarted k3s."
