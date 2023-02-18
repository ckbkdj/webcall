// WebCall Copyright 2022 timur.mobi. All rights reserved.
package main

type DbEntry struct {
	StartTime int64
	Ip string
//	Paassword string        // not used
}

type DbUser struct {
	Name string             // nickname, if given
	Ip1 string              // used for httpRegister
	UserAgent string        // used for httpRegister
	MastodonID string
	Email2 string           // now used as tw_handle
	Str1 string             // now used as tw_user_id
	Str2 string             //  web push device 1 subscription
	Str2ua string           //  web push device 1 user agent
	Str3 string             //  web push device 2 subscription
	Str3ua string           //  web push device 2 user agent
	AltIDs string
	LastLoginTime int64
	LastLogoffTime int64
	Int2 int                // bit 0: hidden callee mode 0/1
	CallCounter int         // incremented by wsHub processTimeValues()
	ConnectedToPeerSecs int // incremented by wsHub processTimeValues()
	LocalP2pCounter int     // incremented by wsHub processTimeValues()
	RemoteP2pCounter int    // incremented by wsHub processTimeValues()
	StoreContacts bool      // TODO could also be encoded in Int2
	StoreMissedCalls bool	// TODO could also be encoded in Int2
	MastodonSendTootOnCall bool
	AskCallerBeforeNotify bool
}

type NotifTweet struct { // key = TweetID string
	TweetTime int64
	Comment string
}

