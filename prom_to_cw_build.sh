#!/usr/bin/env bash
$(aws ecr get-login --no-include-email --region us-west-2)
docker build -t prometheus-to-cloudwatch . --no-cache=true
docker tag prometheus-to-cloudwatch:latest 235895476583.dkr.ecr.us-west-2.amazonaws.com/prometheus-to-cloudwatch:latest
docker push 235895476583.dkr.ecr.us-west-2.amazonaws.com/prometheus-to-cloudwatch:latest