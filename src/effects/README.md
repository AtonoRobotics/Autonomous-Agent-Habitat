# effects

The executor: the only path from a proposed effect to the world.

Every effect passes the policy gateway *and* an approved card or an evidence-backed
grant, and is recorded as `executed` only after a real send through a bound
connector returns 2xx. Writing `executed` without a world send is autonomy theater.
