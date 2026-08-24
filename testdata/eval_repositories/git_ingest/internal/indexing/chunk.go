package indexing

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

type CodeChunk struct {
	ID          string  `json:"id"`
	SnapshotID  string  `json:"snapshot_id"`
	Path        string  `json:"path"`
	Language    string  `json:"language"`
	Symbol      string  `json:"symbol,omitempty"`
	StartLine   int     `json:"start_line"`
	EndLine     int     `json:"end_line"`
	Content     string  `json:"content"`
	ContentHash string  `json:"content_hash"`
	Score       float64 `json:"score,omitempty"`
}

var (
	goSymbolRegex     = regexp.MustCompile(`(?m)^func\s+(?:\([^\)]+\)\s+)?([A-Za-z0-9_]+)\s*\(|^type\s+([A-Za-z0-9_]+)\s+(?:struct|interface)`)
	pythonSymbolRegex = regexp.MustCompile(`(?m)^(?:def|class)\s+([A-Za-z0-9_]+)\s*[\(:]`)
	jsSymbolRegex     = regexp.MustCompile(`(?m)^(?:function|class)\s+([A-Za-z0-9_]+)\s*[\(\{]|(?:const|let|var)\s+([A-Za-z0-9_]+)\s*=\s*(?:function|\([^\)]*\)\s*=>)`)
	javaSymbolRegex   = regexp.MustCompile(`(?m)^(?:public|protected|private|static|\s)+[\w\<\>\[\]]+\s+([A-Za-z0-9_]+)\s*\([^\)]*\)\s*\{|^(?:public|protected|private|\s)*(?:class|interface|enum)\s+([A-Za-z0-9_]+)`)
)

type CodeChunker struct {
	targetChunkLines int
	overlapLines     int
}

func NewCodeChunker(targetLines, overlap int) *CodeChunker {
	if targetLines <= 0 {
		targetLines = 60
	}
	if overlap < 0 || overlap >= targetLines {
		overlap = 10
	}
	return &CodeChunker{
		targetChunkLines: targetLines,
		overlapLines:     overlap,
	}
}

func (c *CodeChunker) ChunkFile(snapshotID, relPath, content string) []CodeChunk {
	lines := strings.Split(content, "\n")
	totalLines := len(lines)
	if totalLines == 0 {
		return nil
	}

	lang := DetectLanguage(relPath)
	var chunks []CodeChunk

	if totalLines <= c.targetChunkLines {
		chunkContent := strings.Join(lines, "\n")
		h := sha256.Sum256([]byte(chunkContent))
		symbol := extractSymbol(chunkContent, lang)
		chunks = append(chunks, CodeChunk{
			ID:          uuid.New().String(),
			SnapshotID:  snapshotID,
			Path:        relPath,
			Language:    lang,
			Symbol:      symbol,
			StartLine:   1,
			EndLine:     totalLines,
			Content:     chunkContent,
			ContentHash: hex.EncodeToString(h[:]),
		})
		return chunks
	}

	step := c.targetChunkLines - c.overlapLines
	for start := 0; start < totalLines; start += step {
		end := start + c.targetChunkLines
		if end > totalLines {
			end = totalLines
		}

		chunkLines := lines[start:end]
		chunkContent := strings.Join(chunkLines, "\n")
		h := sha256.Sum256([]byte(chunkContent))
		symbol := extractSymbol(chunkContent, lang)

		chunks = append(chunks, CodeChunk{
			ID:          uuid.New().String(),
			SnapshotID:  snapshotID,
			Path:        relPath,
			Language:    lang,
			Symbol:      symbol,
			StartLine:   start + 1,
			EndLine:     end,
			Content:     chunkContent,
			ContentHash: hex.EncodeToString(h[:]),
		})

		if end == totalLines {
			break
		}
	}

	return chunks
}

func extractSymbol(content, lang string) string {
	switch lang {
	case "go":
		if m := goSymbolRegex.FindStringSubmatch(content); len(m) > 0 {
			if m[1] != "" {
				return m[1]
			}
			if len(m) > 2 && m[2] != "" {
				return m[2]
			}
		}
	case "python":
		if m := pythonSymbolRegex.FindStringSubmatch(content); len(m) > 1 && m[1] != "" {
			return m[1]
		}
	case "javascript", "typescript":
		if m := jsSymbolRegex.FindStringSubmatch(content); len(m) > 0 {
			for i := 1; i < len(m); i++ {
				if m[i] != "" {
					return m[i]
				}
			}
		}
	case "java":
		if m := javaSymbolRegex.FindStringSubmatch(content); len(m) > 0 {
			for i := 1; i < len(m); i++ {
				if m[i] != "" {
					return m[i]
				}
			}
		}
	}
	return ""
}
