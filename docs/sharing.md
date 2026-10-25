# Sharing

The page can be opened from outside the house through a [Cloudflare Access](https://developers.cloudflare.com/cloudflare-one/policies/access/)
app in front of bifrost, one app per bifrost. With `ACCESS_TEAM_DOMAIN`, `ACCESS_AUD`,
`CLOUDFLARE_ACCOUNT_ID` and `CLOUDFLARE_API_TOKEN` set, the page shows who is signed in, and an admin (`BIFROST_ADMINS`) gets a
Share… button to invite people by email and remove them.

bifrost needs no names. It reads who is looking only from the JWT Access signs into
`Cf-Access-Jwt-Assertion`, checked against the team's keys, never from a header or a host name a
proxy can change. The JWT's `aud` must be `ACCESS_AUD`, the tag on the app's Overview page under
Application Audience (AUD) Tag: bifrost is also reachable without Cloudflare (on the LAN or a
tailnet), where a token the same team issued for another app could otherwise be replayed. The tag
finds the app in the account; the link is the app's domain, and the lists are the Access groups its allow policies include, inline or reusable. A group
another Access app also uses, or one nested in another group, is shown but never edited, since
changing it would change who opens that app too; so is a group one of this app's deny or bypass
policies names. While any policy or rule in the account cannot be read, no group is edited at all. Editing a group only adds or removes its `email` rules; any other rule in it (a whole
domain, a login method) stays as it is. bifrost never creates or deletes groups, policies or apps:
make the app, a policy, and a group of its own for it in Cloudflare once, and manage the people
from the page.

## Invite links

Adding someone by email needs the address they will sign in with, which people often get wrong (a
work Google account, an old address). An invite link needs none: whoever opens it signs in however
they like and is added to the list, by the address Access then vouches for.

Access never lets someone off the list reach the page, so the link lives under a second Access app
on `<domain>/join/*` (without the `/*` it would cover `/join` alone), whose only policy lets in anyone who signs in (`Everyone`, with the same login
methods). bifrost takes that app's tokens (`ACCESS_JOIN_AUD`) on `/join/<token>` and nowhere else.
A link is 24 random bytes, kept only as a hash, made by an admin, good for a week, and used up by
the first person it adds. It is marked used on disk before that person is added, so a failed write
lets nobody in rather than letting a second person in after a restart. Someone already on the list
who opens it (its sender checking it) goes to the page and leaves it unused; the person it let in can
open it again until it expires. Since anyone can sign in to the join app, a wrong or used link is
answered from memory, without a call to Cloudflare. A link to a list another app has started to use
since stops working, as editing it by email would.

## Roles

Each member in the Share dialog has a role: Viewer or Admin. An admin can invite, remove people and
change roles; a viewer only uses the page. Admins made from the dialog are kept in
`STATE_DIR/admins.json` on top of `BIFROST_ADMINS`, whose admins cannot be changed from the page.
Nobody can change their own role, only a member can be made an admin, and removing someone also
drops their admin role.

