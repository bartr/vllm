#!/bin/bash

PROM=$(kubectl -n monitoring get svc prometheus -o jsonpath='{.spec.clusterIP}'):9090
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"kv-node\"}"
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"rtx\"}"
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"cllm-test\"}"
curl -s -X POST -g "http://$PROM/api/v1/admin/tsdb/delete_series?match[]={node=\"smoke-test\"}"
curl -s -X POST    "http://$PROM/api/v1/admin/tsdb/clean_tombstones"
