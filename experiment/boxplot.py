import matplotlib.pyplot as plt
import seaborn as sns
import pandas as pd

proposed_method_interval1 = [
    2.800158, 3.829855, 2.339885, 2.23008, 2.614375,
    4.010416, 3.609663, 3.724294, 3.272349, 4.354821
]

whereby_interval1 = [
    7.762785, 14.849511, 6.86224, 7.648623, 18.018656,
    13.692203, 15.580214, 7.999611, 6.72378, 7.852573
]

teams_interval1 = [
    15.463941, 3.342641, 3.836864, 7.299831, 6.274813,
    5.775729, 3.558481, 5.775729, 7.623545, 11.577602
]

df_data = {
    'Method': ['Proposed Method'] * len(proposed_method_interval1) + \
              ['Whereby'] * len(whereby_interval1) + \
              ['Teams'] * len(teams_interval1),
    'Recovery Time (s)': proposed_method_interval1 + whereby_interval1 + teams_interval1
}
df = pd.DataFrame(df_data)

plt.figure(figsize=(10, 7))
palette = {"Proposed Method": "lightgreen", "Whereby": "skyblue", "Teams": "lightcoral"}
ax = sns.boxplot(x='Method', y='Recovery Time (s)', data=df, palette=palette)

plt.xlabel('Method')
plt.ylabel('Recovery Time (s)')
plt.grid(axis='y', linestyle='--', alpha=0.7)

medians = df.groupby('Method')['Recovery Time (s)'].median()
methods_order = df['Method'].unique()

horizontal_offset = 0.05
vertical_offset = 0.2

for i, method in enumerate(methods_order):
    median_val = medians.loc[[method]].values.item()
    ax.text(i + horizontal_offset, median_val + vertical_offset, f'{median_val:.2f}s',
            horizontalalignment='left', color='black', weight='semibold', fontsize=10)

ymin, ymax = ax.get_ylim()
ax.set_ylim(ymin, ymax * 1.1)

plt.savefig('boxplot.png')
