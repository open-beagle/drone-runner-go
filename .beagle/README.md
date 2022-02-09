# version

```bash
git remote add upstream git@github.com:drone/runner-go.git

git fetch upstream

git merge v1.11.0
```

## debug

```bash
git apply .beagle/0001-add-Machine-to-Filter.patch

go mod tidy
```

## tag

```bash
git tag v1.11.0-beagle

git push --tags
```
