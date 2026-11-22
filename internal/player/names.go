package player

import (
	"crypto/rand"
	"fmt"
	"math/big"
)

// A guest's name is made here and nowhere else: the player cannot enter one
// and a guest cannot change it, so nothing a person typed ever reaches the
// leaderboard and there is no profanity to moderate. Both lists are plain
// words chosen for that; adding to them needs the same care.
//
// 48 adjectives x 48 animals x 10,000 numbers is about 23 million names. A
// clash with an existing username is retried by the caller.
var guestAdjectives = []string{
	"Swift", "Quiet", "Brave", "Calm", "Clever", "Bright", "Bold", "Gentle",
	"Lucky", "Merry", "Nimble", "Proud", "Quick", "Sharp", "Silent", "Steady",
	"Sunny", "Witty", "Eager", "Fair", "Keen", "Lively", "Mellow", "Noble",
	"Patient", "Rapid", "Silver", "Golden", "Amber", "Crimson", "Azure", "Emerald",
	"Frosty", "Misty", "Stormy", "Windy", "Dusty", "Mossy", "Sandy", "Rocky",
	"Cosmic", "Lunar", "Solar", "Polar", "Royal", "Rustic", "Vivid", "Wild",
}

var guestAnimals = []string{
	"Fox", "Owl", "Wolf", "Hawk", "Lynx", "Otter", "Heron", "Raven",
	"Falcon", "Badger", "Beaver", "Bison", "Crane", "Deer", "Eagle", "Finch",
	"Gecko", "Hare", "Ibis", "Jaguar", "Koala", "Lemur", "Marten", "Newt",
	"Ocelot", "Panda", "Quail", "Robin", "Seal", "Tiger", "Urchin", "Viper",
	"Walrus", "Yak", "Zebra", "Moose", "Puffin", "Stork", "Swan", "Tern",
	"Wren", "Bear", "Crow", "Dove", "Elk", "Goose", "Mole", "Orca",
}

// newGuestName returns a name like "SwiftFox4821".
func newGuestName() (string, error) {
	adjective, err := randomBelow(len(guestAdjectives))
	if err != nil {
		return "", err
	}
	animal, err := randomBelow(len(guestAnimals))
	if err != nil {
		return "", err
	}
	number, err := randomBelow(10000)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s%s%04d", guestAdjectives[adjective], guestAnimals[animal], number), nil
}

func randomBelow(n int) (int, error) {
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0, err
	}
	return int(v.Int64()), nil
}
