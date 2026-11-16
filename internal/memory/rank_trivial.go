package memory

// greetings are the openers that carry no retrievable content on their own.
var greetings = map[string]bool{
	"hi": true, "hii": true, "hey": true, "heya": true, "hello": true, "hallo": true,
	"yo": true, "sup": true, "ok": true, "okay": true, "k": true, "kk": true,
	"thanks": true, "thank": true, "thx": true, "ty": true, "cheers": true,
	"yes": true, "yeah": true, "yep": true, "no": true, "nope": true, "nah": true,
	"bye": true, "ciao": true, "adios": true, "hola": true, "buenas": true,
	"gracias": true, "si": true, "vale": true, "good": true, "nice": true,
	"lol": true, "haha": true, "morning": true, "gm": true, "gn": true,
}

// Trivial reports whether a prompt is too thin to be worth a recall lookup:
// pure punctuation or emoji, a single very short word, or a greeting
// (docs/memory.md, "skip recall for trivial prompts"). Callers skip the
// retrieval path when it is true; it never affects what is stored.
func Trivial(text string) bool {
	words := Words(text)
	if len(words) == 0 {
		return true // punctuation, emoji, whitespace
	}
	if len(words) > 3 {
		return false
	}
	// A short single token ("hm", "42") says nothing to search for.
	if len(words) == 1 && countRunes(words[0]) <= 2 {
		return true
	}
	for _, w := range words {
		if !greetings[w] {
			return false
		}
	}
	return true
}

func countRunes(s string) int {
	n := 0
	for range s {
		n++
	}
	return n
}
