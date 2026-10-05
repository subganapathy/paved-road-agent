My general understanding of AI agents is that domain knowledge expressed as system prompts and tools gives a new programming language for it to solve problems in that domain. 

Concretely, the "domain" here is safe modification of production. What is a production modification?

- Code changes
- K8s manifest changes { source controlled }
- Cloud infra changes { again source controlled via tf or cross plane }
- Dynamic config changes { hopefully source controlled as well }

We make the assumption that the runtime fleet is a set of kubernetes managed clusters. What this assumption buys us? It greatly reduces the surface areas in terms of discovering workloads, how they communicate with each other, how to secure the supply chain, how they access cloud resources, how the said cloud resources are mutated/modified.

Let's focus on the code change surface area for a saas/data plane. Typically, it's a business logic fix. A business logic fix can conflict with the expected input { in terms of mismatched assumptions between proto vs impl or adr vs impl }. Furthermore, a business logic fix might need to be rolled out in a safe manner { critical fix and might need safe rollout and the application doesn't have a canary style rollout } . Next comes any potential missing assumption in the csp infra as well as misaligned dependency invocation { say networkpolicy issues, authn/z /mesh missed onboarding }. Finally, comes the reservations vs actual vs new tps and a warning. Similarly, a change might take a dependency on a new client, reliable efforts to detect whether contractually our integration is valid, next based on the data sent/received are there enough authn/z and encryption in transit to protect this data.  Lastly is the server scaled both vertically and horizontally to handle the request { roughly p99 to service and estimation of cpu used and memory used to potential new load to estimate to the number of replicas and size of 1 replica  }. Next some infra changes. TF, cross plane or pulumi or whatever. This is precisely where our assumption of k8s pays off and my thesis about agents being a programmer who builds using the libraries and stdlibs { model trained inputs } pays off.  In kubernetes, there is one or few ways to federate into a cloud and access csp resources. In kubernwetes, there is some well trodden paths for s2s comm and each of these paths have well-defined metrics { istio, linkerd, appmesh etc,. etc,. }. Same with service deployments and csp resource upgrade { assume gitops => Maybe kubernetes + every change via source control incl dynamic configs }.

Separately with any pr, it might make sense for the agent to review the kubernetes deployment configs and point out any hardening gaps and tie it the change.

Next comes the infra review, critical eye needs to be placed on quotas and the impact of how the provider applies the change and partial failures { replace policy would delete and re create what about partial failures }