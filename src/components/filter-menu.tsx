import { useState, type ReactNode } from "react";
import { Check, ChevronDown } from "lucide-react";
import { Button } from "@/components/ui/button";
import {
  Popover,
  PopoverContent,
  PopoverTrigger,
} from "@/components/ui/popover";
import {
  Command,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from "@/components/ui/command";
import { cn } from "@/lib/utils";

export type FilterOption = { value: string; label: string; count: number };
export function FilterMenu({
  label,
  icon,
  options,
  selected,
  onChange,
}: {
  label: string;
  icon: ReactNode;
  options: FilterOption[];
  selected: string[];
  onChange: (values: string[]) => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <Popover open={open} onOpenChange={setOpen}>
      <PopoverTrigger asChild>
        <Button
          variant="outline"
          className={cn(
            "filter-trigger",
            selected.length > 0 && "filter-active",
          )}
          aria-label={`${label}筛选${selected.length ? `，已选 ${selected.length} 项` : ""}`}
        >
          {icon}
          <span>{label}</span>
          {selected.length > 0 && (
            <span className="filter-count">{selected.length}</span>
          )}
          <ChevronDown className="ml-1 size-3.5 text-muted-foreground" />
        </Button>
      </PopoverTrigger>
      <PopoverContent align="start" className="w-64 p-0">
        <Command>
          <CommandInput placeholder={`搜索${label}…`} />
          <CommandList>
            <CommandEmpty>没有匹配选项</CommandEmpty>
            <CommandGroup>
              {options.map((o) => (
                <CommandItem
                  key={o.value}
                  value={`${o.value} ${o.label}`}
                  onSelect={() =>
                    onChange(
                      selected.includes(o.value)
                        ? selected.filter((v) => v !== o.value)
                        : [...selected, o.value],
                    )
                  }
                >
                  <span
                    className={cn(
                      "filter-check",
                      selected.includes(o.value) && "checked",
                    )}
                  >
                    {selected.includes(o.value) && <Check className="size-3" />}
                  </span>
                  <span>{o.label}</span>
                  <span className="ml-auto text-xs tabular-nums text-muted-foreground">
                    {o.count}
                  </span>
                </CommandItem>
              ))}
            </CommandGroup>
          </CommandList>
        </Command>
        {selected.length > 0 && (
          <div className="border-t p-1">
            <Button
              variant="ghost"
              size="sm"
              className="w-full"
              onClick={() => onChange([])}
            >
              清除选择
            </Button>
          </div>
        )}
      </PopoverContent>
    </Popover>
  );
}
