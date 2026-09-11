# Fixture for the ruby preset (mache-34c926). Two classes share a method
# name, a module declares one, and one def sits at the top level — so the
# projection has to say which of them each method belongs to, and must not
# project any of them twice.

require 'set'

module Registry
  def register(x)
    x
  end
end

class Catalog
  include Registry

  def insert(entry)
    entry
  end

  def count
    0
  end
end

class Index
  def count
    1
  end
end

def top_level_helper
  2
end
