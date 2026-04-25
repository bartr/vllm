#!/bin/bash

set -euo pipefail

kubectl apply -k "$(dirname "$0")"
