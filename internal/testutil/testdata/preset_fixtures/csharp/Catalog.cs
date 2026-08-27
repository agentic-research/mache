using System;
using System.Collections.Generic;

namespace Mache.Fixture
{
    public enum EntryKind
    {
        File,
        Directory,
        Symlink,
    }

    public struct EntryStats
    {
        public long Size;
        public DateTime ModifiedAt;
    }

    public interface ILookup
    {
        Entry? Lookup(string id);
        int Count { get; }
    }

    public class Entry
    {
        public string Id { get; set; } = string.Empty;
        public EntryKind Kind { get; set; }
    }

    public class Catalog : ILookup
    {
        private readonly Dictionary<string, Entry> _entries = new();
        private readonly int _capacity;

        public int Count => _entries.Count;
        public int Capacity => _capacity;

        public Catalog(int capacity)
        {
            _capacity = capacity;
        }

        public bool Insert(Entry entry)
        {
            if (_entries.Count >= _capacity)
            {
                return false;
            }
            _entries[entry.Id] = entry;
            return true;
        }

        public Entry? Lookup(string id)
        {
            return _entries.TryGetValue(id, out var value) ? value : null;
        }
    }
}
