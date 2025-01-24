import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';

interface Store {
  bears: number;
  increase: (by: number) => void;
}

export const useStore = create<Store>()(
  devtools(
    persist(
      (set) => ({
        bears: 0,
        increase: (by) => set((state) => ({ bears: state.bears + by })),
      }),
      { name: 'bearStore' }
    )
  )
);
