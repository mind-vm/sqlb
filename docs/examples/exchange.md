# exchange — money, both ways

> **Where the source is.** This example lives outside the sqlb repository and is not published yet, so there is nothing to link to. The code quoted below is real; the paths are given so they can be found once it is.

Fictional companies, prices that wander on a clock, and anybody may trade. One
schema declaration produces the tables, the models, the REST API and its OpenAPI
document; the trading rules live in write hooks, so they apply to every path
that creates an order rather than to the one endpoint somebody remembered.

The market opens with the process: prices move every two seconds whether or not
anyone is watching.

## What it is arguing

[library](library.md) is about a resource that must not be
over-committed. This is the same question asked of money, which is harder in
three ways:

- **the invariant is two-sided.** A trade moves cash one way and shares the
  other. Both halves move in one transaction or neither does, or the exchange
  has invented value;
- **the commitment outlives the request.** A resting limit order is a promise to
  pay made now and kept minutes later, so the money is *reserved* at placement
  rather than merely checked. A balance that is only checked is a balance two
  open orders can both spend;
- **the price is not an input.** It moves on a clock the client does not
  control, and moves again as a consequence of the client's own order. No
  request can name the price it executes at, and no column holding a price is
  writable through the API.

## A minute with curl

```bash
base=http://localhost:8080/api

trader=$(curl -sX POST $base/traders -H 'content-type: application/json' \
    -d '{"name":"Ada","email":"ada@example.com"}' | jq -r .id)

curl -sX POST $base/traders/$trader/deposit -H 'content-type: application/json' \
    -d '{"amount_cents":10000000}'                       # $100,000, out of thin air

stock=$(curl -s "$base/stocks?symbol=eq.AZR" | jq -r .items[0].id)

curl -sX POST $base/orders -H 'content-type: application/json' \
    -d "{\"trader_id\":\"$trader\",\"stock_id\":\"$stock\",
         \"side\":\"buy\",\"kind\":\"market\",\"quantity\":10}"
```

The order comes back already filled — the response carries `filled_quantity`,
`status` and `closed_at`, because the engine ran inside the insert.

A limit order is the interesting one. Bid below the market and it rests, with
the cash reserved, until the random walk comes to it:

```bash
curl -sX POST $base/orders -H 'content-type: application/json' \
    -d "{\"trader_id\":\"$trader\",\"stock_id\":\"$stock\",\"side\":\"buy\",
         \"kind\":\"limit\",\"quantity\":5,\"limit_price_cents\":17500}"

curl -s "$base/traders/$trader"          # reserved_cents is now 87500
curl -s "$base/orders?status=eq.open&expand=stock"
curl -s "$base/ticks?stock_id=eq.$stock&sort=-at&per_page=20"   # the chart
```

A real run of that, with the tick interval at 300ms:

```
market at 17706, bidding 17528
placed: open, 0 filled        reserved: 87640
t+1s  price=17740  order=filled 5
trade: buy 5 @ 17215
```

Two things in that trace are the design, not an accident. The fill happened at
17215 rather than at the 17528 that was bid — a limit is the worst price
accepted, not the price agreed. And the chart shows 17215 followed immediately
by 17217: the tick moved the price, the trade moved it again.

## Three details worth stealing

**An insert can mean something.** `POST /api/orders` is a *generated* handler.
It decodes a body, validates it and inserts a row, and it knows nothing about
money. The `AfterCreate` hook turns that insert into a placement — reserve,
match, write the trade — inside the same transaction, so a refusal rolls the
order row back with it. That is why the schema has no "rejected" status: an
order that could not be placed is not an order, it is a 422.

The alternative was a hand-written `/orders/place`, which would have worked and
would have been a **second door**: the generated create would still exist, and
the next writer of orders would insert rows that reserved nothing.

**The lock you did not ask for.** Two concurrent orders in one stock deadlocked,
every time, before `market.Prepare` existed. Inserting an order takes a
`FOR KEY SHARE` lock on the stock and trader rows it references — Postgres
checking the foreign keys, invisible in the Go code — and both transactions then
tried to upgrade the same row to `FOR UPDATE`. Taking the exclusive lock in
`BeforeCreate`, before the row exists, collapses it into a queue. The rule is
easy to state and impossible to see, which is why it is a named function with
the explanation attached.

**Three layers, and only one of them can be forgotten.** The check constraints
(`cash_cents >= 0`, `reserved_cents <= cash_cents`) cannot be bypassed by code
that has not been written yet. The row locks make the arithmetic correct. The Go
validation exists so that the answer is a 422 naming the field rather than a 500
naming a constraint. Removing the third is a worse API; removing the first is a
bug waiting for a Friday.

## The market model, stated plainly

This is **not a limit order book**. There is one counterparty — the house — and
it always quotes the current price. A real book matches buyers against sellers,
and an example built that way deadlocks on its first user: one person placing
one order has nobody to trade with, so nothing fills and nothing gets exercised.

A **market order** fills in full, immediately, at the price on the screen — its
only limit is the account balance. A **resting limit order** fills at most
`tick_liquidity` shares per tick, so an order larger than the depth fills across
several ticks at several prices, which is where partial fills come from.

What that costs, said out loud rather than hidden: there is no spread, no queue
priority, and no depth beyond one number per stock. Prices follow a lognormal
random walk with mild mean reversion towards the day's open — reversion is not
realism, it is what stops an unattended demo from being a page of stocks that
have all wandered to a cent by morning.

Money is integer cents everywhere, and every column carrying it says so in its
name. Not `float64`, because a binary float cannot represent 0.10 and the
disagreement lands in somebody's balance.

## Where each rule lives

| | |
|---|---|
| `exchangeschema/schema.go` | Six tables, the capabilities each column opts into, and the check constraints that are the floor under everything else |
| `market/engine.go` | Placement, settlement, cancellation. Every balance change in the system is in this file |
| `market/ticker.go` | The clock: a lognormal random walk, and the resting orders each new price reaches |
| `app/hooks.go` | Two registrations that turn `POST /api/orders` from an insert into a placement |
| `app/orders.go`, `traders.go`, `stocks.go` | The five endpoints that are state transitions rather than column assignments |

Everything else — filtering, sorting, search, pagination, `?expand=stock`, the
OpenAPI document — is generated from the schema and appears as a single
`exchange.Register(api, db)`.

## Not built, deliberately

No authentication: a trader is a row, and any caller may act as any of them.
[tasks](tasks.md) is the one with a JWT and a tenant boundary, and
duplicating it here would bury what this example is about. **Do not put this on
the internet.**

No realised profit-and-loss. It needs a cost-basis policy — FIFO, average,
specific lot — and picking one silently would be worse than not having it.

No session close. `open_cents` and `volume` are set when a stock is listed and
never reset, so "the day" is the lifetime of the database.

## The test to read first

`TestConcurrentBuyersCannotSpendTheSameCash`. Twenty requests, each affordable
alone, arrive at once against a balance covering half. It does not assert how
many succeed — that would be testing the Go scheduler — only that the books
balance: no cash invented, none destroyed, and every share held justified by a
trade.

## Next

- [Hooks](../queries/hooks.md) — `AfterCreate`, transactions, and lock ordering
- [Where domain logic goes](../concepts/domain-logic.md) — the four places a
  rule can live
