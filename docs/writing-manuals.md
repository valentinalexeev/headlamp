# Writing manuals

Write all manuals in this repository in Simplified Technical English (STE), to the rules of ASD-STE100. Readers of these manuals are often not native English speakers, and they can be at work during a failure. STE gives them one clear meaning for each sentence.

This page gives the rules that we use. It does not replace the specification. ASD gives the specification and its dictionary at no cost, on request, at <https://www.asd-ste100.org>.

## What is a manual

These files are manuals:

- All `README.md` files.
- All files in `docs/`, except `docs/superpowers/`.
- `VERSIONING.md`.
- The "Bootstrap your own infra" section of `CLAUDE.md`.

These files are not manuals: `CHANGELOG.md`, `THIRD_PARTY.md`, the agent rules in `CLAUDE.md` and `AGENTS.md`, code comments, and commit messages.

## Rules

### Words

1. Use approved words from the STE dictionary, and use each word with its approved meaning only. For example, "close" means "move to shut". Do not use it to mean "near".
2. Use one word for one thing. If you call a part a "bridge", do not also call it an "ingress" in the same manual.
3. You can use technical names and technical verbs that are specific to this project. Examples are `headscale`, "tailnet", "exit node" and "deploy". Write commands, file names and settings as code.
4. Do not use a noun group of more than three words. Divide it with "of" or "for".
5. Use the article ("the", "a") where it is possible.

### Sentences

6. Write one instruction in one sentence.
7. Write an instruction as a command: "Stop the node". Use a numbered list for steps in a sequence.
8. Do not write an instruction with more than 20 words.
9. Do not write a description with more than 25 words.
10. Use the active voice. Use the passive only when you do not know who or what does the action.
11. Use the present, past and future tenses only. Do not write "has stopped". Write "stopped".
12. Do not use "-ing" words, except in technical names. Write "when you start the node", not "when starting the node".
13. Do not use contractions or Latin abbreviations. Do not add polite words to a command.

### Paragraphs

14. Write one topic in one paragraph.
15. Do not write a paragraph with more than six sentences.

### Warnings

16. Put a warning or a caution before the step that it applies to.
17. Start with the instruction, then give the reason:

    > **WARNING:** Do not mark a bridge as intermittent if it must always apply revocations. A bridge that the tailnet cannot reach can continue to serve clients.

18. Use **WARNING** for a risk to security or to user access. Use **CAUTION** for a risk to data or to a service. Use **NOTE** for other information.

## The check

`scripts/check-manual-style.py` checks the rules that a script can read: rules 8, 9, 13 and 15, and a short list of words to replace. CI runs it on each manual that a pull request changes. Run it yourself:

```sh
make check-manuals
```

The script does not read the dictionary, the voice or the meaning of a sentence. A manual that passes can still break the rules, so read it again after the check.

## Accuracy comes first

A shorter sentence must keep the full technical meaning. Keep all values, limits, conditions, warnings and security notes. If a fact does not fit in one sentence, use two sentences. Do not remove it.
