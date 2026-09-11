namespace Mache.Fixture
{
    // A record declares methods and properties too. The type-qualified
    // selectors have to cover record_declaration or a record's members are
    // projected nowhere at all (mache-34c926).
    public record Point(int X, int Y)
    {
        public int Manhattan() => X + Y;

        public int Total { get; }
    }

    public record struct Span(int Lo, int Hi)
    {
        public int Width() => Hi - Lo;
    }
}
