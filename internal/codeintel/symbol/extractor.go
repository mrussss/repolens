package symbol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"go/ast"
	"go/format"
	"go/token"
	"strings"

	"repolens/internal/codeintel/model"
)

// ExtractSymbols extracts all functions, methods, types, and interfaces from a parsed AST file.
func ExtractSymbols(fset *token.FileSet, astFile *ast.File, filePath, modulePath, packagePath string, content []byte) []*model.Symbol {
	if astFile == nil {
		return nil
	}

	var symbols []*model.Symbol
	pkgName := astFile.Name.Name

	ast.Inspect(astFile, func(n ast.Node) bool {
		if n == nil {
			return true
		}

		switch decl := n.(type) {
		case *ast.FuncDecl:
			sym := extractFuncDecl(fset, decl, filePath, modulePath, packagePath, pkgName, content)
			if sym != nil {
				symbols = append(symbols, sym)
			}
			return false // Do not inspect inside func body for top-level symbols

		case *ast.GenDecl:
			if decl.Tok == token.TYPE {
				for _, spec := range decl.Specs {
					if typeSpec, ok := spec.(*ast.TypeSpec); ok {
						sym := extractTypeSpec(fset, decl, typeSpec, filePath, modulePath, packagePath, pkgName, content)
						if sym != nil {
							symbols = append(symbols, sym)
						}
					}
				}
				return false
			}
		}

		return true
	})

	return symbols
}

func extractFuncDecl(fset *token.FileSet, decl *ast.FuncDecl, filePath, modulePath, packagePath, pkgName string, content []byte) *model.Symbol {
	name := decl.Name.Name
	startPos := fset.Position(decl.Pos())
	endPos := fset.Position(decl.End())

	spanBytes := getSourceSpan(content, startPos, endPos)
	contentHash := sha256.Sum256(spanBytes)
	contentHashHex := hex.EncodeToString(contentHash[:])

	var docStr string
	if decl.Doc != nil {
		docStr = decl.Doc.Text()
	}

	// Signature
	sigBuf := new(bytes.Buffer)
	// Temporarily clear body for clean signature formatting
	origBody := decl.Body
	decl.Body = nil
	_ = format.Node(sigBuf, fset, decl)
	decl.Body = origBody
	sig := strings.TrimSpace(sigBuf.String())

	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		// Normal Function
		rawKey, hashKey := model.BuildSymbolKey(modulePath, packagePath, "", model.SymbolKindFunction, name)
		qualName := pkgName + "." + name

		return &model.Symbol{
			FilePath:          filePath,
			SymbolKeyRaw:      rawKey,
			SymbolKeyHash:     hashKey,
			ModulePath:        modulePath,
			PackagePath:       packagePath,
			PackageName:       pkgName,
			Kind:              model.SymbolKindFunction,
			Name:              name,
			QualifiedName:     qualName,
			Signature:         sig,
			Doc:               docStr,
			StartLine:         startPos.Line,
			StartCol:          startPos.Column,
			EndLine:           endPos.Line,
			EndCol:            endPos.Column,
			Exported:          ast.IsExported(name),
			ContentHash:       contentHashHex,
			SourceExcerpt:     string(spanBytes),
		}
	}

	// Method
	recvField := decl.Recv.List[0]
	recvBuf := new(bytes.Buffer)
	_ = format.Node(recvBuf, fset, recvField.Type)
	receiverRaw := recvBuf.String()
	receiverCanonical := model.CanonicalizeReceiver(receiverRaw)

	rawKey, hashKey := model.BuildSymbolKey(modulePath, packagePath, receiverCanonical, model.SymbolKindMethod, name)
	qualName := pkgName + "." + receiverCanonical + "." + name

	return &model.Symbol{
		FilePath:          filePath,
		SymbolKeyRaw:      rawKey,
		SymbolKeyHash:     hashKey,
		ModulePath:        modulePath,
		PackagePath:       packagePath,
		PackageName:       pkgName,
		Kind:              model.SymbolKindMethod,
		Name:              name,
		QualifiedName:     qualName,
		ReceiverRaw:       receiverRaw,
		ReceiverCanonical: receiverCanonical,
		Signature:         sig,
		Doc:               docStr,
		StartLine:         startPos.Line,
		StartCol:          startPos.Column,
		EndLine:           endPos.Line,
		EndCol:            endPos.Column,
		Exported:          ast.IsExported(name),
		ContentHash:       contentHashHex,
		SourceExcerpt:     string(spanBytes),
	}
}

func extractTypeSpec(fset *token.FileSet, decl *ast.GenDecl, typeSpec *ast.TypeSpec, filePath, modulePath, packagePath, pkgName string, content []byte) *model.Symbol {
	name := typeSpec.Name.Name
	startPos := fset.Position(typeSpec.Pos())
	endPos := fset.Position(typeSpec.End())

	spanBytes := getSourceSpan(content, startPos, endPos)
	contentHash := sha256.Sum256(spanBytes)
	contentHashHex := hex.EncodeToString(contentHash[:])

	var docStr string
	if typeSpec.Doc != nil {
		docStr = typeSpec.Doc.Text()
	} else if decl.Doc != nil {
		docStr = decl.Doc.Text()
	}

	kind := model.SymbolKindType
	if _, isInterface := typeSpec.Type.(*ast.InterfaceType); isInterface {
		kind = model.SymbolKindInterface
	}

	// Format signature / type definition
	typeBuf := new(bytes.Buffer)
	_ = format.Node(typeBuf, fset, typeSpec)
	sig := strings.TrimSpace(typeBuf.String())

	rawKey, hashKey := model.BuildSymbolKey(modulePath, packagePath, "", kind, name)
	qualName := pkgName + "." + name

	return &model.Symbol{
		FilePath:      filePath,
		SymbolKeyRaw:  rawKey,
		SymbolKeyHash: hashKey,
		ModulePath:    modulePath,
		PackagePath:   packagePath,
		PackageName:   pkgName,
		Kind:          kind,
		Name:          name,
		QualifiedName: qualName,
		Signature:     "type " + sig,
		Doc:           docStr,
		StartLine:     startPos.Line,
		StartCol:      startPos.Column,
		EndLine:       endPos.Line,
		EndCol:        endPos.Column,
		Exported:      ast.IsExported(name),
		ContentHash:   contentHashHex,
		SourceExcerpt: string(spanBytes),
	}
}

func getSourceSpan(content []byte, startPos, endPos token.Position) []byte {
	if len(content) == 0 {
		return nil
	}
	startOffset := startPos.Offset
	endOffset := endPos.Offset

	if startOffset < 0 {
		startOffset = 0
	}
	if endOffset > len(content) {
		endOffset = len(content)
	}
	if startOffset >= endOffset || startOffset >= len(content) {
		return nil
	}
	return content[startOffset:endOffset]
}
