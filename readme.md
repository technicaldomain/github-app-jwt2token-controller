## GitHub App jwt2token kubernetes controller


## ArgoCD repository password update

```yaml
apiVersion: githubapp.kharkevich.org/v1
kind: ArgoCDRepo
metadata:
  name: example-argo-github-app
  namespace: default
spec:
  privateKeySecret: github-app-private-key
  argoCDRepositories:
    - repository: repo-1294444111
      namespace: argocd
```

## Docker config generation

```yaml
apiVersion: githubapp.kharkevich.org/v1
kind: DockerConfigJson
metadata:
  name: example-docker-config
  namespace: default
spec:
  privateKeySecret: github-app-private-key
  dockerConfigSecrets:
    - secret: ghcr
      namespace: argocd
```


For more information see `artifacts` directory


## License

Apache 2 Licensed. For more information please see [LICENSE](LICENSE)
