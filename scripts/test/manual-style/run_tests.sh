#!/usr/bin/env bash
# Tests for the manual style check.
set -uo pipefail

check="$(cd "$(dirname "$0")/../../.." && pwd)/scripts/check-manual-style.py"
dir=$(mktemp -d)
trap 'rm -rf "$dir"' EXIT
failed=0

words() { printf 'word %.0s' $(seq 1 "$1") | sed 's/ $//'; }
long=$(words 30)
twenty_one=$(words 21)

ok() { # NAME TEXT
	printf '%b\n' "$2" >"$dir/a.md"
	if out=$("$check" "$dir/a.md" 2>&1); then echo "ok   $1"; else echo "FAIL $1: $out"; failed=1; fi
}
no() { # NAME TEXT EXPECTED
	printf '%b\n' "$2" >"$dir/a.md"
	if out=$("$check" "$dir/a.md" 2>&1); then
		echo "FAIL $1: passed"
		failed=1
	elif ! grep -q -- "$3" <<<"$out"; then
		echo "FAIL $1: $out"
		failed=1
	else
		echo "ok   $1"
	fi
}

ok 'short text' 'Stop the node. The node stops.'
no 'long description' "$long." '30 words, limit 25'
ok 'description at 21 words' "$twenty_one."
no 'long instruction' "1. $twenty_one." '21 words, limit 20'
ok 'bullet uses the description limit' "- $twenty_one."
no 'too many sentences' 'One. Two. Three. Four. Five. Six. Seven.' '7 sentences'
ok 'six sentences' 'One. Two. Three. Four. Five. Six.'
ok 'list items are not one paragraph' '- One. Two. Three.\n- Four. Five. Six.\n- Seven.'
no 'contraction' 'It does not work and we cannot use what won'"'"'t start.' 'contraction'
no 'word to replace' 'Ensure that it runs.' 'make sure'
no 'word inside a sentence' 'It goes via the proxy.' 'through'
no 'abbreviation' 'Use a value, e.g. this one.' 'for example'
ok 'code is one word' "Run \`$long\` now."
ok 'a code block is left out' "\`\`\`\n$long.\n\`\`\`"
ok 'a table is left out' "| $long. |"
ok 'a heading is left out' "# $long."
ok 'a comment is left out' "<!-- $long. -->"
ok 'a link counts as its text' 'See [the guide](https://example.com/a/b/c/d/e/f/g/h) now.'
# shellcheck disable=SC2016 # literal backticks, the check reads them
ok 'code with a banned word' 'Set `ensure` to true.'
ok 'a hyphenated word' 'This is a via-point.'
no 'wrapped lines are one paragraph' "$twenty_one\nmore words here and here and here." 'limit 25'

exit $failed
