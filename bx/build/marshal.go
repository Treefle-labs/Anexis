package build

import "gopkg.in/yaml.v3"

// UnmarshalYAML handle the case which `build: ./context` and `build: {context: ...}`
func (cb *ComposeBuild) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode { // Case build: ./context
		cb.Context = value.Value
		return nil
	}
	// Case build: { ... } (map)
	// use a temp type to avoid the infinite recursion
	type ComposeBuildMap struct {
		Context    string             `yaml:"context,omitempty"`
		Dockerfile string             `yaml:"dockerfile,omitempty"`
		Args       map[string]*string `yaml:"args,omitempty"`
		Target     string             `yaml:"target,omitempty"`
		CacheFrom  []string           `yaml:"cache_from,omitempty"`
		Labels     map[string]string  `yaml:"labels,omitempty"`
		Network    string             `yaml:"network,omitempty"`
	}
	var temp ComposeBuildMap
	if err := value.Decode(&temp); err != nil {
		return err
	}
	cb.Context = temp.Context
	cb.Dockerfile = temp.Dockerfile
	cb.Args = temp.Args
	cb.Target = temp.Target
	cb.CacheFrom = temp.CacheFrom
	cb.Labels = temp.Labels
	cb.Network = temp.Network

	// Apply the default if context is empty but build is a non empty map
	if cb.Context == "" && !value.IsZero() && value.Kind == yaml.MappingNode {
		cb.Context = "."
	}
	return nil
}