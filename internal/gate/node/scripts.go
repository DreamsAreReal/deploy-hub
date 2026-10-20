package node

import (
	"os"
	"path/filepath"
	"strings"
)

const (
	eslintConfigFile      = "eslint.config.mjs"
	complexityConfigFile  = "complexity.config.mjs"
	fuzzSetupFile         = "fuzz-setup.mjs"
	fuzzSetupCJS          = "fuzz-setup.cjs"
	syntaxCheckFile       = "syntax-check.mjs"
	knipConfigFile        = "knip.json"
	HTMLValidateConfig    = "htmlvalidate.json"
	StylelintConfig       = "stylelint.json"
	scriptPerm            = 0o600
	complexityPlaceholder = "__LIMIT__"
)

const eslintConfig = `import js from "@eslint/js";
import sonarjs from "eslint-plugin-sonarjs";
import tseslint from "typescript-eslint";

export default tseslint.config(
  { ignores: ["**/node_modules/**", "**/dist/**", "**/build/**", "**/coverage/**"] },
  js.configs.recommended,
  ...tseslint.configs.strict,
  sonarjs.configs.recommended,
  {
    linterOptions: { noInlineConfig: true, reportUnusedDisableDirectives: "error" },
    rules: {
      "sonarjs/cognitive-complexity": ["error", __LIMIT__],
      "no-magic-numbers": ["error", { ignore: [0, 1, -1], ignoreArrayIndexes: true, ignoreDefaultValues: true, enforceConst: true }],
      "no-console": "error",
    },
  },
  {
    files: ["**/*.test.*", "**/*.spec.*", "**/test/**", "**/tests/**", "**/__tests__/**"],
    rules: { "no-magic-numbers": "off", "sonarjs/no-duplicate-string": "off" },
  },
);
`

const complexityConfig = `import sonarjs from "eslint-plugin-sonarjs";
import tseslint from "typescript-eslint";

export default [
  { ignores: ["**/node_modules/**", "**/dist/**", "**/build/**", "**/coverage/**"] },
  {
    files: ["**/*.{js,mjs,cjs,jsx,ts,mts,cts,tsx}"],
    languageOptions: { parser: tseslint.parser },
    linterOptions: { noInlineConfig: true },
    plugins: { sonarjs },
    rules: { "sonarjs/cognitive-complexity": ["error", __LIMIT__] },
  },
];
`

const fuzzSetup = `import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { dirname, join } from "node:path";
import { pathToFileURL } from "node:url";

const manifest = createRequire(join(process.cwd(), "package.json")).resolve("fast-check/package.json");
const entry = JSON.parse(readFileSync(manifest, "utf8")).exports["."].import.default;
const fc = await import(pathToFileURL(join(dirname(manifest), entry)).href);
fc.configureGlobal({ numRuns: Number(process.env.DEPLOY_HUB_FUZZ_RUNS) });
`

const fuzzSetupRequire = `require("fast-check").configureGlobal({ numRuns: Number(process.env.DEPLOY_HUB_FUZZ_RUNS) });
`

const syntaxCheck = `import { readFileSync } from "node:fs";
import { createRequire } from "node:module";
import { extname } from "node:path";

const esbuild = createRequire(import.meta.url)("esbuild");
const file = process.argv[2];
const loaders = { ".ts": "ts", ".mts": "ts", ".cts": "ts", ".tsx": "tsx", ".jsx": "jsx" };
esbuild.transformSync(readFileSync(file, "utf8"), { loader: loaders[extname(file)] ?? "js", logLevel: "silent" });
`

const knipConfig = `{}
`

const htmlValidateConfig = `{"extends": ["html-validate:recommended"], "rules": {"require-sri": ["error", {"target": "crossorigin"}]}}
`

const stylelintConfig = `{"extends": ["stylelint-config-standard"]}
`

func writeScripts(dir string, limit int) error {
	files := map[string]string{
		eslintConfigFile:     eslintConfig,
		complexityConfigFile: complexityConfig,
		fuzzSetupFile:        fuzzSetup,
		fuzzSetupCJS:         fuzzSetupRequire,
		syntaxCheckFile:      syntaxCheck,
		knipConfigFile:       knipConfig,
		HTMLValidateConfig:   htmlValidateConfig,
		StylelintConfig:      stylelintConfig,
	}
	for name, content := range files {
		text := strings.ReplaceAll(content, complexityPlaceholder, itoa(limit))
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), scriptPerm); err != nil {
			return err
		}
	}
	return nil
}
