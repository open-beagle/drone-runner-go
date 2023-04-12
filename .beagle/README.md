# drone/runner-go

<https://github.com/drone/runner-go>

```bash
git remote add upstream git@github.com:drone/runner-go.git

git fetch upstream

git merge v1.12.0
```

## patch

```bash
git apply .beagle/0001-add-Machine-to-Filter.patch

go mod tidy
```

## tag

```bash
git tag v1.12.0-beagle.1

git push origin v1.12.0-beagle.1
```
