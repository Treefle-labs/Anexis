package services

type BuildMetaData struct {
	BaseImage string           `json:"base_image"`
	Entry     []DirEntry       `json:"entry"`
	Specs     map[string]Specs `json:"specs"`
}

type DirEntry struct {
	IsFile      bool       `json:"is_file"`
	IsDir       bool       `json:"is_dir"`
	FileContent []byte     `json:"file_content"`
	FileName    string     `json:"file_name"`
	Entries     []DirEntry `json:"entries"`
}

type Specs struct {
	Type string `json:"type"`
}

const (
	COMMAND = "Command"
	EXPR    = "Expression"
	ANNOTATION = "Annotation"
)
