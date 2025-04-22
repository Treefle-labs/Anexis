package types

type BuildSpec struct {
    Name    string `yaml:"name"`
    Version string `yaml:"version"`
    Build   []struct {
        Path       string `yaml:"path"`
        Dockerfile string `yaml:"dockerfile"`
        Tag        string `yaml:"tag"`
    } `yaml:"build"`
    Output []struct {
        Type string `yaml:"type"`
        Name string `yaml:"name"`
    } `yaml:"output"`
}
