---
issue: 463
type: task
section: Changed
status: done
title: Go module updates
components: [node-control, funnel-ingress]
pr: 463
---
## Summary

The Go modules of node-control and funnel-ingress use newer minor and patch versions of the AWS SDK for Go v2
(`config`, `credentials`, `autoscaling`, `ec2`, `route53`) and `golang.org/x/oauth2` 0.38.0. No setting changes.
