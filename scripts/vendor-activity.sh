#!/bin/sh
# Rebuilds verify/static, the files the Discord Activity serves itself.
#
# Inside Discord the material page can only reach its own origin, so pdf.js,
# the Embedded App SDK and the fonts that the web page loads from CDNs are
# vendored here and embedded into the binary. Run it from the repository root
# after changing a version below, then update the paths in
# verify/materials.html and verify/activity.go to match.
set -eu

PDFJS=4.10.38
SDK=2.5.0
ESBUILD=0.24.2
FONTS=5.3.0

out=verify/static
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

rm -rf "$out"
mkdir -p "$out"

# pdf.js: the same legacy build the web page pins with SRI.
(cd "$work" && npm pack --silent "pdfjs-dist@$PDFJS" >/dev/null && mkdir pdfjs && tar xzf "pdfjs-dist-$PDFJS.tgz" -C pdfjs)
p="$out/pdfjs-$PDFJS"
mkdir -p "$p/legacy/build"
cp "$work/pdfjs/package/legacy/build/pdf.min.mjs" "$work/pdfjs/package/legacy/build/pdf.worker.min.mjs" "$p/legacy/build/"
cp -r "$work/pdfjs/package/cmaps" "$work/pdfjs/package/standard_fonts" "$p/"
cp "$work/pdfjs/package/LICENSE" "$p/LICENSE"

# The SDK is published as modules with dependencies; bundle it into one file.
mkdir "$work/sdk"
(cd "$work/sdk" && npm init -y >/dev/null &&
	npm install --silent "@discord/embedded-app-sdk@$SDK" "esbuild@$ESBUILD" >/dev/null &&
	echo 'export { DiscordSDK } from "@discord/embedded-app-sdk";' >entry.mjs &&
	npx esbuild entry.mjs --bundle --format=esm --minify --platform=browser --target=es2020 \
		--legal-comments=eof --log-level=warning --outfile=sdk.mjs)
cp "$work/sdk/sdk.mjs" "$out/sdk-$SDK.mjs"

# The page's Google Fonts, as woff2 in the Latin and Greek subsets.
f="$out/fonts-$FONTS"
mkdir -p "$f"
: >"$f/fonts.css"
for pkg in fira-sans:400,500,700 fira-sans-extra-condensed:700,800 jetbrains-mono:500,700; do
	name=${pkg%%:*}
	(cd "$work" && npm pack --silent "@fontsource/$name@$FONTS" >/dev/null && mkdir "$name" && tar xzf "fontsource-$name-$FONTS.tgz" -C "$name")
	cp "$work/$name/package/LICENSE" "$f/LICENSE-$name"
	for w in $(echo "${pkg#*:}" | tr , ' '); do
		# Keep the @font-face blocks of the wanted subsets, woff2 only.
		awk -v keep='-(latin|latin-ext|greek|greek-ext)-[0-9]+-normal \\*/' '
			/^\/\*/ { on = ($0 ~ keep) }
			on {
				sub(/, url\([^)]*\.woff\) format\(.woff.\)/, "")
				sub(/url\(\.\/files\//, "url(")
				print
			}' "$work/$name/package/$w.css" >>"$f/fonts.css"
		for s in latin latin-ext greek greek-ext; do
			src="$work/$name/package/files/$name-$s-$w-normal.woff2"
			if [ -f "$src" ]; then cp "$src" "$f/"; fi
		done
	done
done

du -sh "$out"
